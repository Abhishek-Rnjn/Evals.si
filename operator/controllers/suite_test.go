package controllers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1/evalsiv1alpha1connect"
	v1 "github.com/abhishek-rnjn/evals.si/operator/api/v1alpha1"
	evalsiwebhook "github.com/abhishek-rnjn/evals.si/operator/webhook"
)

// fakeAPI stands in for evalsid: runs advance one state per GetRun.
type fakeAPI struct {
	evalsiv1alpha1connect.UnimplementedRunServiceHandler
	evalsiv1alpha1connect.UnimplementedMonitorServiceHandler
	mu        sync.Mutex
	runs      map[string]*evalsiv1alpha1.Run
	creates   []*evalsiv1alpha1.CreateRunRequest
	cancelled []string
	policies  map[string]*evalsiv1alpha1.OnlineEvalPolicy
	applied   []*evalsiv1alpha1.OnlineEvalPolicy
	deleted   []string
	// Runs whose spec has this judge stay running.
	holdJudge string
	tokens    []string
}

func (f *fakeAPI) token(h http.Header) {
	f.tokens = append(f.tokens, h.Get("Authorization"))
}

func (f *fakeAPI) CreateRun(_ context.Context, req *connect.Request[evalsiv1alpha1.CreateRunRequest]) (*connect.Response[evalsiv1alpha1.CreateRunResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.token(req.Header())
	f.creates = append(f.creates, req.Msg)
	id := "run-" + string(rune('a'+len(f.runs)))
	r := &evalsiv1alpha1.Run{Id: id, Name: req.Msg.GetName(), Project: req.Msg.GetProject(), Spec: req.Msg.GetSpec(), Status: evalsiv1alpha1.RunStatus_RUN_STATUS_PENDING}
	f.runs[id] = r
	return connect.NewResponse(&evalsiv1alpha1.CreateRunResponse{Run: r}), nil
}

func (f *fakeAPI) GetRun(_ context.Context, req *connect.Request[evalsiv1alpha1.GetRunRequest]) (*connect.Response[evalsiv1alpha1.GetRunResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.runs[req.Msg.GetId()]
	if r == nil {
		return nil, connect.NewError(connect.CodeNotFound, nil)
	}
	switch r.Status {
	case evalsiv1alpha1.RunStatus_RUN_STATUS_PENDING:
		r.Status, r.StartedAt = evalsiv1alpha1.RunStatus_RUN_STATUS_RUNNING, timestamppb.Now()
		r.Progress = &evalsiv1alpha1.Progress{Total: 15, Done: 3}
	case evalsiv1alpha1.RunStatus_RUN_STATUS_RUNNING:
		if r.GetSpec().GetJudge() == f.holdJudge {
			break
		}
		r.Status, r.FinishedAt = evalsiv1alpha1.RunStatus_RUN_STATUS_SUCCEEDED, timestamppb.Now()
		r.Progress = &evalsiv1alpha1.Progress{Total: 15, Done: 15}
		r.Summaries = []*evalsiv1alpha1.MetricSummary{{Metric: "exact-match", N: 15, Mean: proto.Float64(0.9333333), Ci: &evalsiv1alpha1.ConfidenceInterval{Low: 0.7, High: 0.99}}}
		r.Gates = []*evalsiv1alpha1.GateResult{{Gate: &evalsiv1alpha1.Gate{Metric: "exact-match"}, Passed: true}}
	}
	return connect.NewResponse(&evalsiv1alpha1.GetRunResponse{Run: proto.Clone(r).(*evalsiv1alpha1.Run)}), nil
}

func (f *fakeAPI) CancelRun(_ context.Context, req *connect.Request[evalsiv1alpha1.CancelRunRequest]) (*connect.Response[evalsiv1alpha1.CancelRunResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelled = append(f.cancelled, req.Msg.GetId())
	r := f.runs[req.Msg.GetId()]
	r.Status = evalsiv1alpha1.RunStatus_RUN_STATUS_CANCELLED
	return connect.NewResponse(&evalsiv1alpha1.CancelRunResponse{Run: r}), nil
}

func (f *fakeAPI) ApplyPolicy(_ context.Context, req *connect.Request[evalsiv1alpha1.ApplyPolicyRequest]) (*connect.Response[evalsiv1alpha1.ApplyPolicyResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := req.Msg.GetPolicy()
	f.policies[p.GetName()] = p
	f.applied = append(f.applied, p)
	return connect.NewResponse(&evalsiv1alpha1.ApplyPolicyResponse{Policy: p}), nil
}

func (f *fakeAPI) ListPolicies(context.Context, *connect.Request[evalsiv1alpha1.ListPoliciesRequest]) (*connect.Response[evalsiv1alpha1.ListPoliciesResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := &evalsiv1alpha1.ListPoliciesResponse{}
	for _, p := range f.policies {
		out.Policies = append(out.Policies, p)
	}
	return connect.NewResponse(out), nil
}

func (f *fakeAPI) DeletePolicy(_ context.Context, req *connect.Request[evalsiv1alpha1.DeletePolicyRequest]) (*connect.Response[evalsiv1alpha1.DeletePolicyResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.policies, req.Msg.GetName())
	f.deleted = append(f.deleted, req.Msg.GetName())
	return connect.NewResponse(&evalsiv1alpha1.DeletePolicyResponse{}), nil
}

func (f *fakeAPI) GetPolicyStats(_ context.Context, req *connect.Request[evalsiv1alpha1.GetPolicyStatsRequest]) (*connect.Response[evalsiv1alpha1.GetPolicyStatsResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.policies[req.Msg.GetName()] == nil {
		return nil, connect.NewError(connect.CodeNotFound, nil)
	}
	return connect.NewResponse(&evalsiv1alpha1.GetPolicyStatsResponse{Stats: &evalsiv1alpha1.PolicyStats{Policy: req.Msg.GetName(), TracesSeen: 40, TracesEvaluated: 4}}), nil
}

type env struct {
	api   *fakeAPI
	admin client.Client
	// Clients acting as users, through the webhooks.
	as func(user string) client.Client
}

// start runs an API server (envtest) with the CRDs and webhooks installed,
// the operator's controllers against a fake evalsid, and returns clients.
func start(t *testing.T) *env {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		if _, err := os.Stat("/opt/envtest/bin/kube-apiserver"); err != nil {
			t.Skip("KUBEBUILDER_ASSETS is not set (setup-envtest use -p path)")
		}
		t.Setenv("KUBEBUILDER_ASSETS", "/opt/envtest/bin")
	}
	fake := &fakeAPI{runs: map[string]*evalsiv1alpha1.Run{}, policies: map[string]*evalsiv1alpha1.OnlineEvalPolicy{}, holdJudge: "hold"}
	mux := http.NewServeMux()
	mux.Handle(evalsiv1alpha1connect.NewRunServiceHandler(fake))
	mux.Handle(evalsiv1alpha1connect.NewMonitorServiceHandler(fake))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "config", "crd"), "testdata"},
		ErrorIfCRDPathMissing: true,
		WebhookInstallOptions: envtest.WebhookInstallOptions{Paths: []string{filepath.Join("..", "config", "webhook")}},
	}
	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = testEnv.Stop() })

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1.AddToScheme(scheme)
	wh := testEnv.WebhookInstallOptions
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		WebhookServer: webhook.NewServer(webhook.Options{
			Host: wh.LocalServingHost, Port: wh.LocalServingPort, CertDir: wh.LocalServingCertDir,
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(tokenFile, []byte("sa-token\n"), 0o600)
	if err := Setup(mgr, Config{
		Enabled: []string{"evalrun", "onlineevalpolicy", "evaluator", "sandboxclass"},
		API:     APIConfig{URL: srv.URL, TokenFile: tokenFile}, PollInterval: 200 * time.Millisecond, StatsInterval: 200 * time.Millisecond,
		Namespace: "evalsi", Image: "ghcr.io/abhishek-rnjn/evalsi:test", SandboxTLSSecret: "sandbox-tls",
		SandboxAllowClients: []string{"spiffe://evals.si/ns/evalsi/sa/evalsi-worker"}, SandboxServiceAccount: "evalsi-sandboxd",
		WorkerConfigMap: "evalsi-worker", NATSMonitoringEndpoint: "evalsi-nats.evalsi.svc:8222",
	}); err != nil {
		t.Fatal(err)
	}
	evalsiwebhook.Register(mgr.GetWebhookServer())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := mgr.Start(ctx); err != nil {
			t.Error(err)
		}
	}()
	t.Cleanup(func() { cancel(); <-done })

	admin, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	return &env{api: fake, admin: admin, as: func(user string) client.Client {
		u, err := testEnv.AddUser(envtest.User{Name: user, Groups: []string{"system:masters"}}, &rest.Config{})
		if err != nil {
			t.Fatal(err)
		}
		c, err := client.New(u.Config(), client.Options{Scheme: scheme})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}}
}

// eventually polls cond for up to 20s.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
