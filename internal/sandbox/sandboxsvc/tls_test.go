package sandboxsvc

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	sandboxv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/sandbox/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/sandbox/v1alpha1/sandboxv1alpha1connect"
	"github.com/abhishek-rnjn/evals.si/internal/sandbox"
)

type ca struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
	n    int64
}

func newCA(t *testing.T) *ca {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ca"}, IsCA: true, BasicConstraintsValid: true,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(c)
	return &ca{cert: c, key: key, pool: pool, n: 1}
}

func (c *ca) leaf(t *testing.T, cn, spiffe string) tls.Certificate {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	c.n++
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(c.n), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	if spiffe != "" {
		u, _ := url.Parse(spiffe)
		tmpl.URIs = []*url.URL{u}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func TestServeTLSAdmitsOnlyAllowedClients(t *testing.T) {
	authority := newCA(t)
	server := authority.leaf(t, "sandboxd", "")
	sb, err := sandbox.New(sandbox.Config{WorkDir: t.TempDir(), CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	m := sandbox.NewManager(sb)
	defer m.Close()
	if _, _, err := ServeTLS(context.Background(), m, "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{server}}, nil); err == nil {
		t.Fatal("served without mutual TLS")
	}
	cfg := &tls.Config{Certificates: []tls.Certificate{server}, ClientCAs: authority.pool, ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS12}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop, addr, err := ServeTLS(ctx, m, "127.0.0.1:0", cfg, []string{"spiffe://evals.si/worker"})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	call := func(client *tls.Certificate) error {
		tc := &tls.Config{RootCAs: authority.pool, MinVersion: tls.VersionTLS12}
		if client != nil {
			tc.Certificates = []tls.Certificate{*client}
		}
		hc := &http.Client{Transport: &http.Transport{TLSClientConfig: tc, ForceAttemptHTTP2: true}}
		c := sandboxv1alpha1connect.NewSandboxServiceClient(hc, "https://"+addr, connect.WithGRPC())
		_, err := c.Probe(context.Background(), connect.NewRequest(&sandboxv1alpha1.ProbeRequest{}))
		return err
	}
	if err := call(nil); err == nil {
		t.Error("a client without a certificate got in")
	}
	other := authority.leaf(t, "other", "spiffe://evals.si/other")
	if err := call(&other); err == nil || !strings.Contains(err.Error(), "403") && connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a client not on the list: %v", err)
	}
	worker := authority.leaf(t, "worker", "spiffe://evals.si/worker")
	if err := call(&worker); err != nil {
		t.Errorf("the allowed worker: %v", err)
	}
}
