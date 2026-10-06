package e2e

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/structpb"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1/evalsiv1alpha1connect"
	"github.com/abhishek-rnjn/evals.si/internal/auth"
	"github.com/abhishek-rnjn/evals.si/internal/config"
	"github.com/abhishek-rnjn/evals.si/internal/sandbox"
	"github.com/abhishek-rnjn/evals.si/internal/sandbox/sandboxsvc"
)

// testPKI writes a CA and certificates it signs into dir: name.crt and
// name.key for each leaf. A leaf gets the IP 127.0.0.1 and, when spiffe is
// set, that SPIFFE ID.
type testPKI struct {
	dir    string
	ca     *x509.Certificate
	caKey  *ecdsa.PrivateKey
	serial int64
}

func newPKI(t *testing.T) *testPKI {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "evalsi test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(der)
	p := &testPKI{dir: t.TempDir(), ca: ca, caKey: key, serial: 1}
	writePEM(t, filepath.Join(p.dir, "ca.crt"), "CERTIFICATE", der)
	return p
}

func writePEM(t *testing.T, path, kind string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (p *testPKI) leaf(t *testing.T, name, spiffe string) (cert, key string) {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	p.serial++
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(p.serial), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{name},
	}
	if spiffe != "" {
		u, _ := url.Parse(spiffe)
		tmpl.URIs = []*url.URL{u}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.ca, &k.PublicKey, p.caKey)
	if err != nil {
		t.Fatal(err)
	}
	kder, _ := x509.MarshalECPrivateKey(k)
	cert, key = filepath.Join(p.dir, name+".crt"), filepath.Join(p.dir, name+".key")
	writePEM(t, cert, "CERTIFICATE", der)
	writePEM(t, key, "EC PRIVATE KEY", kder)
	return cert, key
}

// TestRemoteSandboxService: workers that hold credentials run no sandboxes;
// agent sandboxes and code evaluators go to sandboxd (the Kubernetes sandbox
// pool) over mutual TLS, pinned to the worker's SPIFFE ID.
func TestRemoteSandboxService(t *testing.T) {
	needSandbox(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pki := newPKI(t)
	serverCert, serverKey := pki.leaf(t, "sandboxd", "")
	workerCert, workerKey := pki.leaf(t, "worker", "spiffe://evals.si/ns/evalsi/sa/evalsi-worker")
	tlsCfg, err := auth.ServerTLS(&auth.TLSConfig{CertFile: serverCert, KeyFile: serverKey, ClientCA: filepath.Join(pki.dir, "ca.crt"), RequireClientCert: true})
	if err != nil {
		t.Fatal(err)
	}
	sb, err := sandbox.New(sandbox.Config{WorkDir: t.TempDir(), CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	pool := sandbox.NewManager(sb)
	defer pool.Close()
	stop, addr, err := sandboxsvc.ServeTLS(ctx, pool, "127.0.0.1:0", tlsCfg, []string{"spiffe://evals.si/ns/evalsi/sa/evalsi-worker"})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	e := start(t, func(c *config.Config) {
		c.Worker.SandboxService = &config.SandboxService{
			Address: "tls://" + addr, CAFile: filepath.Join(pki.dir, "ca.crt"), CertFile: workerCert, KeyFile: workerKey,
		}
	})

	// An agent run: the CLI agent and the checker run in the pool's sandbox.
	runs := evalsiv1alpha1connect.NewRunServiceClient(h2cClient(), e.base, connect.WithGRPC())
	created, err := runs.CreateRun(ctx, connect.NewRequest(&evalsiv1alpha1.CreateRunRequest{Spec: &evalsiv1alpha1.RunSpec{
		Target: &evalsiv1alpha1.Target{Agent: &evalsiv1alpha1.AgentTarget{Kind: &evalsiv1alpha1.AgentTarget_Cli{Cli: &evalsiv1alpha1.CLIAgent{
			Command: []string{"sh", "-c", "cat >/dev/null; echo done > out.txt"},
		}}}},
		Environment: &evalsiv1alpha1.Environment{
			Sandbox: &evalsiv1alpha1.SandboxPolicy{MinIsolation: "namespaced"},
			Checker: &evalsiv1alpha1.Checker{Command: []string{"sh", "-c", "test \"$(cat out.txt)\" = done"}},
		},
		Dataset:    &evalsiv1alpha1.DatasetSource{Source: &evalsiv1alpha1.DatasetSource_Inline{Inline: &evalsiv1alpha1.InlineRecords{Records: []*evalsiv1alpha1.Record{{Id: "t", Input: text("Write done to out.txt.")}}}}},
		Evaluators: []*evalsiv1alpha1.EvaluatorRef{{Ref: "task-success"}},
	}}))
	if err != nil {
		t.Fatal(err)
	}
	final := watch(t, runs, created.Msg.GetRun().GetId())
	if final.GetStatus() != evalsiv1alpha1.RunStatus_RUN_STATUS_SUCCEEDED || mean(final, "task-success") != 1 {
		t.Fatalf("agent run: %v %s %v", final.GetStatus(), final.GetError(), final.GetSummaries())
	}
	afterAgent := pool.Created()
	if afterAgent == 0 {
		t.Fatal("the agent's sandbox was not created in the pool")
	}

	// A code evaluator: the unit tests run in the pool too.
	eval := evalsiv1alpha1connect.NewEvaluationServiceClient(h2cClient(), e.base, connect.WithGRPC())
	resp, err := eval.Evaluate(ctx, connect.NewRequest(&evalsiv1alpha1.EvaluateRequest{
		Records: []*evalsiv1alpha1.Record{{
			Id: "good", Output: text("def add(a, b):\n    return a + b\n"), Reference: text("def check(f):\n    assert f(2, 3) == 5\n"),
			Metadata: map[string]*structpb.Value{"entry_point": structpb.NewStringValue("add")},
		}},
		Evaluators: []*evalsiv1alpha1.EvaluatorRef{{Ref: "unit-tests"}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if r := resp.Msg.GetResults()[0]; r.GetOutcome() != evalsiv1alpha1.Outcome_OUTCOME_SCORED || !r.GetScores()[0].GetPassed() {
		t.Fatalf("unit tests: %v %s", r.GetOutcome(), r.GetReason())
	}
	if pool.Created() <= afterAgent {
		t.Error("the code evaluator's sandbox was not created in the pool")
	}
}
