// Package sandboxsvc serves evalsi.sandbox.v1alpha1.SandboxService over a
// sandbox.Manager. evalsid serves it to its workers on a private Unix socket;
// it is never mounted on the public API port.
package sandboxsvc

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	sandboxv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/sandbox/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/sandbox/v1alpha1/sandboxv1alpha1connect"
	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/sandbox"
)

// Service implements sandboxv1alpha1connect.SandboxServiceHandler.
type Service struct {
	m *sandbox.Manager
}

func New(m *sandbox.Manager) *Service { return &Service{m: m} }

// Handler returns the service's path and HTTP handler.
func (s *Service) Handler() (string, http.Handler) {
	return sandboxv1alpha1connect.NewSandboxServiceHandler(s)
}

// Serve listens on a Unix socket (created 0600 in a 0700 directory) until
// ctx ends. It returns once the socket is accepting.
func Serve(ctx context.Context, m *sandbox.Manager, socket string) (stop func(), err error) {
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		return nil, err
	}
	_ = os.Remove(socket)
	ln, err := net.Listen("unix", socket)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle(New(m).Handler())
	p := new(http.Protocols)
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)
	srv := &http.Server{Handler: mux, Protocols: p, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = srv.Shutdown(sctx)
			_ = os.Remove(socket)
		})
	}
	go func() {
		<-ctx.Done()
		stop()
	}()
	return stop, nil
}

func toErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, sandbox.ErrNoSnapshots):
		return connect.NewError(connect.CodeUnimplemented, err)
	case errors.Is(err, sandbox.ErrUnavailable):
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("SANDBOX_UNAVAILABLE: "+err.Error()))
	case errors.Is(err, sandbox.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeDeadlineExceeded, err)
	}
	return connect.NewError(connect.CodeInvalidArgument, err)
}

var levels = map[string]evalsiv1alpha1.IsolationLevel{
	"none": evalsiv1alpha1.IsolationLevel_ISOLATION_LEVEL_NONE, "confined": evalsiv1alpha1.IsolationLevel_ISOLATION_LEVEL_CONFINED,
	"namespaced": evalsiv1alpha1.IsolationLevel_ISOLATION_LEVEL_NAMESPACED, "kernel": evalsiv1alpha1.IsolationLevel_ISOLATION_LEVEL_KERNEL,
	"vm": evalsiv1alpha1.IsolationLevel_ISOLATION_LEVEL_VM,
}

// Report converts an isolation to the proto form recorded on records.
func Report(iso sandbox.Isolation) *evalsiv1alpha1.IsolationReport {
	enf := evalsiv1alpha1.Enforcement_ENFORCEMENT_FULL
	if iso.Enforcement == "partial" {
		enf = evalsiv1alpha1.Enforcement_ENFORCEMENT_PARTIAL
	}
	return &evalsiv1alpha1.IsolationReport{Driver: iso.Driver, Level: levels[iso.Level], Enforcement: enf, Notes: iso.Notes}
}

func (s *Service) Probe(ctx context.Context, _ *connect.Request[sandboxv1alpha1.ProbeRequest]) (*connect.Response[sandboxv1alpha1.ProbeResponse], error) {
	out := &sandboxv1alpha1.ProbeResponse{}
	for _, r := range s.m.Sandbox().Probe(ctx) {
		out.Rungs = append(out.Rungs, &sandboxv1alpha1.RungStatus{Driver: r.Driver, Level: levels[r.Level], Available: r.Available, Reason: r.Reason})
	}
	return connect.NewResponse(out), nil
}

var networkModes = map[sandboxv1alpha1.NetworkMode]string{
	sandboxv1alpha1.NetworkMode_NETWORK_MODE_UNSPECIFIED: sandbox.NetworkDeny,
	sandboxv1alpha1.NetworkMode_NETWORK_MODE_DENY:        sandbox.NetworkDeny,
	sandboxv1alpha1.NetworkMode_NETWORK_MODE_ALLOWLIST:   sandbox.NetworkAllowlist,
	sandboxv1alpha1.NetworkMode_NETWORK_MODE_ALLOW:       sandbox.NetworkAllow,
}

// SpecFromProto converts a sandbox spec.
func SpecFromProto(p *sandboxv1alpha1.SandboxSpec) *sandbox.Spec {
	sp := &sandbox.Spec{
		Image: p.GetImage(), ReadOnlyRoot: p.GetReadOnlyRoot(), MinIsolation: p.GetMinIsolation(), Mode: sandbox.Mode(p.GetMode()),
		Network: sandbox.Network{Mode: networkModes[p.GetNetwork().GetMode()], Allow: p.GetNetwork().GetAllow()},
		Resources: sandbox.Resources{
			MemoryMB: int(p.GetResources().GetMemoryMb()), MaxProcs: int(p.GetResources().GetMaxProcs()),
			MaxFileMB: int(p.GetResources().GetMaxFileMb()), CPUSeconds: int(p.GetResources().GetCpuSeconds()),
		},
		Env: p.GetEnv(), Files: p.GetFiles(), Workdir: p.GetWorkdir(), IdleTimeout: p.GetIdleTimeout().AsDuration(),
	}
	return sp
}

func (s *Service) Create(ctx context.Context, req *connect.Request[sandboxv1alpha1.CreateRequest]) (*connect.Response[sandboxv1alpha1.CreateResponse], error) {
	sess, err := s.m.Create(ctx, SpecFromProto(req.Msg.GetSpec()))
	if err != nil {
		return nil, toErr(err)
	}
	return connect.NewResponse(&sandboxv1alpha1.CreateResponse{SandboxId: sess.ID, Isolation: Report(sess.Isolation()), ImageDigest: sess.ImageDigest()}), nil
}

// streamWriter sends output chunks; stdout and stderr are copied from
// separate goroutines, so sends are serialized.
type streamWriter struct {
	mu     *sync.Mutex
	stream *connect.ServerStream[sandboxv1alpha1.ExecResponse]
	stderr bool
}

func (w *streamWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	chunk := append([]byte(nil), p...)
	resp := &sandboxv1alpha1.ExecResponse{Event: &sandboxv1alpha1.ExecResponse_Stdout{Stdout: chunk}}
	if w.stderr {
		resp.Event = &sandboxv1alpha1.ExecResponse_Stderr{Stderr: chunk}
	}
	if err := w.stream.Send(resp); err != nil {
		return 0, err
	}
	return len(p), nil
}

var outcomes = map[string]sandboxv1alpha1.Outcome{
	sandbox.OutcomeExit: sandboxv1alpha1.Outcome_OUTCOME_EXIT, sandbox.OutcomeDenied: sandboxv1alpha1.Outcome_OUTCOME_DENIED,
	sandbox.OutcomeTimeout: sandboxv1alpha1.Outcome_OUTCOME_TIMEOUT, sandbox.OutcomeRunnerFailure: sandboxv1alpha1.Outcome_OUTCOME_RUNNER_FAILURE,
}

func (s *Service) Exec(ctx context.Context, req *connect.Request[sandboxv1alpha1.ExecRequest], stream *connect.ServerStream[sandboxv1alpha1.ExecResponse]) error {
	sess, err := s.m.Get(req.Msg.GetSandboxId())
	if err != nil {
		return toErr(err)
	}
	e := &sandbox.Exec{
		Command: req.Msg.GetCommand(), Stdin: req.Msg.GetStdin(), Env: req.Msg.GetEnv(),
		Timeout: req.Msg.GetTimeout().AsDuration(), OutputLimit: int(req.Msg.GetOutputLimitBytes()), Cwd: req.Msg.GetCwd(),
	}
	mu := &sync.Mutex{}
	res := sess.Exec(ctx, e, &streamWriter{mu: mu, stream: stream}, &streamWriter{mu: mu, stream: stream, stderr: true})
	mu.Lock()
	defer mu.Unlock()
	return stream.Send(&sandboxv1alpha1.ExecResponse{Event: &sandboxv1alpha1.ExecResponse_Result{Result: &sandboxv1alpha1.ExecResult{
		Outcome: outcomes[res.Outcome], ExitCode: int32(res.ExitCode), Duration: durationpb.New(res.Duration),
		Truncated: res.Truncated, Denials: res.Denials, Error: res.Error,
	}}})
}

func (s *Service) WriteFiles(_ context.Context, req *connect.Request[sandboxv1alpha1.WriteFilesRequest]) (*connect.Response[sandboxv1alpha1.WriteFilesResponse], error) {
	sess, err := s.m.Get(req.Msg.GetSandboxId())
	if err != nil {
		return nil, toErr(err)
	}
	files := make([]sandbox.File, len(req.Msg.GetFiles()))
	for i, f := range req.Msg.GetFiles() {
		files[i] = sandbox.File{Path: f.GetPath(), Content: f.GetContent(), Mode: os.FileMode(f.GetMode()).Perm()}
	}
	if err := sess.WriteFiles(files); err != nil {
		return nil, toErr(err)
	}
	return connect.NewResponse(&sandboxv1alpha1.WriteFilesResponse{}), nil
}

func (s *Service) ReadFiles(_ context.Context, req *connect.Request[sandboxv1alpha1.ReadFilesRequest]) (*connect.Response[sandboxv1alpha1.ReadFilesResponse], error) {
	sess, err := s.m.Get(req.Msg.GetSandboxId())
	if err != nil {
		return nil, toErr(err)
	}
	files, missing, truncated, err := sess.ReadFiles(req.Msg.GetPaths(), req.Msg.GetMaxBytes())
	if err != nil {
		return nil, toErr(err)
	}
	out := &sandboxv1alpha1.ReadFilesResponse{Missing: missing, Truncated: truncated}
	for _, f := range files {
		out.Files = append(out.Files, &sandboxv1alpha1.File{Path: f.Path, Content: f.Content, Mode: uint32(f.Mode)})
	}
	return connect.NewResponse(out), nil
}

func (s *Service) Snapshot(ctx context.Context, req *connect.Request[sandboxv1alpha1.SnapshotRequest]) (*connect.Response[sandboxv1alpha1.SnapshotResponse], error) {
	id, err := s.m.Snapshot(ctx, req.Msg.GetSandboxId())
	if err != nil {
		return nil, toErr(err)
	}
	return connect.NewResponse(&sandboxv1alpha1.SnapshotResponse{SnapshotId: id}), nil
}

func (s *Service) Restore(ctx context.Context, req *connect.Request[sandboxv1alpha1.RestoreRequest]) (*connect.Response[sandboxv1alpha1.RestoreResponse], error) {
	var network *sandbox.Network
	if n := req.Msg.GetNetwork(); n != nil {
		network = &sandbox.Network{Mode: networkModes[n.GetMode()], Allow: n.GetAllow()}
	}
	sess, err := s.m.Restore(ctx, req.Msg.GetSnapshotId(), network)
	if err != nil {
		return nil, toErr(err)
	}
	return connect.NewResponse(&sandboxv1alpha1.RestoreResponse{SandboxId: sess.ID, Isolation: Report(sess.Isolation())}), nil
}

func (s *Service) Stats(_ context.Context, req *connect.Request[sandboxv1alpha1.StatsRequest]) (*connect.Response[sandboxv1alpha1.StatsResponse], error) {
	sess, err := s.m.Get(req.Msg.GetSandboxId())
	if err != nil {
		return nil, toErr(err)
	}
	execs, cpu, rss := sess.Stats()
	out := &sandboxv1alpha1.StatsResponse{Execs: execs, CpuTime: durationpb.New(cpu), MaxRssBytes: rss}
	for _, ev := range sess.Egress() {
		out.Egress = append(out.Egress, &sandboxv1alpha1.EgressEvent{
			Time: timestamppb.New(ev.Time), Host: ev.Host, Port: int32(ev.Port), Allowed: ev.Allowed,
			BytesSent: ev.BytesSent, BytesReceived: ev.BytesReceived,
		})
	}
	return connect.NewResponse(out), nil
}

func (s *Service) Destroy(_ context.Context, req *connect.Request[sandboxv1alpha1.DestroyRequest]) (*connect.Response[sandboxv1alpha1.DestroyResponse], error) {
	if err := s.m.Destroy(req.Msg.GetSandboxId()); err != nil {
		return nil, toErr(err)
	}
	return connect.NewResponse(&sandboxv1alpha1.DestroyResponse{}), nil
}

func (s *Service) DeleteSnapshot(_ context.Context, req *connect.Request[sandboxv1alpha1.DeleteSnapshotRequest]) (*connect.Response[sandboxv1alpha1.DeleteSnapshotResponse], error) {
	if err := s.m.DeleteSnapshot(req.Msg.GetSnapshotId()); err != nil {
		return nil, toErr(err)
	}
	return connect.NewResponse(&sandboxv1alpha1.DeleteSnapshotResponse{}), nil
}

// ServeTLS listens on a TCP address with mutual TLS until ctx ends: the
// sandbox pool and the sandboxd DaemonSet, which workers on other pods
// reach. Every client must present a certificate from the configured CA;
// with allowed set, its SPIFFE ID (or subject common name) must be listed.
// It returns the bound address.
func ServeTLS(ctx context.Context, m *sandbox.Manager, addr string, tlsCfg *tls.Config, allowed []string) (stop func(), bound string, err error) {
	if tlsCfg == nil || tlsCfg.ClientAuth != tls.RequireAndVerifyClientCert {
		return nil, "", errors.New("sandbox service on TCP needs mutual TLS (a client CA, client certificates required)")
	}
	ln, err := tls.Listen("tcp", addr, tlsCfg)
	if err != nil {
		return nil, "", err
	}
	mux := http.NewServeMux()
	mux.Handle(New(m).Handler())
	var h http.Handler = mux
	if len(allowed) > 0 {
		ok := map[string]bool{}
		for _, a := range allowed {
			ok[a] = true
		}
		h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 || !ok[peerID(r.TLS.PeerCertificates[0])] {
				http.Error(w, "client not allowed", http.StatusForbidden)
				return
			}
			mux.ServeHTTP(w, r)
		})
	}
	p := new(http.Protocols)
	p.SetHTTP1(true)
	p.SetHTTP2(true)
	srv := &http.Server{Handler: h, Protocols: p, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = srv.Shutdown(sctx)
		})
	}
	go func() {
		<-ctx.Done()
		stop()
	}()
	return stop, ln.Addr().String(), nil
}

// peerID is a client certificate's SPIFFE ID, or its subject common name.
func peerID(c *x509.Certificate) string {
	for _, u := range c.URIs {
		if u.Scheme == "spiffe" {
			return u.String()
		}
	}
	return c.Subject.CommonName
}
