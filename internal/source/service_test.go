package source

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/credentials"
)

func TestCheckEndpoint(t *testing.T) {
	allow := []string{"mlflow.studio.svc", "*.corp.example.com"}
	for _, ok := range []string{
		"https://mlflow.example.com", "https://mlflow.example.com:8443/base", "http://mlflow.studio.svc:5000",
		"http://tracking.corp.example.com", "https://203.0.113.9",
	} {
		if err := CheckEndpoint(ok, allow); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for bad, want := range map[string]string{
		"http://mlflow.example.com":          "must be https",
		"http://127.0.0.1:5000":              "allow_hosts",
		"https://localhost":                  "allow_hosts",
		"https://169.254.169.254/latest":     "allow_hosts",
		"https://10.0.0.5":                   "allow_hosts",
		"https://metadata.google.internal":   "allow_hosts",
		"https://other.studio.svc":           "allow_hosts",
		"https://mlflow":                     "allow_hosts",
		"https://user:pw@mlflow.example.com": "credentials",
		"ftp://mlflow.example.com":           "http(s)",
		"mlflow.example.com":                 "http(s)",
	} {
		err := CheckEndpoint(bad, allow)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want an error mentioning %q", bad, err, want)
		}
	}
}

func TestResolverChecksGrantsThenReadsTheValue(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "mlflow-token"), 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "mlflow-token", "token")
	if err := os.WriteFile(file, []byte("file-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STUDIO_MLFLOW_TOKEN", "env-secret")
	enforce := true
	policy := credentials.NewPolicy(credentials.Settings{Credentials: credentials.Config{Enforce: &enforce, Grants: []credentials.Grant{
		{Env: "STUDIO_MLFLOW_TOKEN", Projects: []string{"studio"}, Hosts: []string{"mlflow.example.com"}},
		{Env: "file:mlflow-token/token", Projects: []string{"studio"}},
	}}})
	resolve := NewResolver(policy, dir)
	ctx := context.Background()
	src := func(c *evalsiv1alpha1.SourceCredentials, project, endpoint string) *evalsiv1alpha1.TraceSource {
		return &evalsiv1alpha1.TraceSource{Name: "s", Project: project, Endpoint: endpoint, Credentials: c}
	}

	if v, err := resolve(ctx, src(nil, "studio", "https://mlflow.example.com")); err != nil || v != "" {
		t.Fatalf("no credentials: %q %v", v, err)
	}
	if v, err := resolve(ctx, src(&evalsiv1alpha1.SourceCredentials{Env: "STUDIO_MLFLOW_TOKEN"}, "studio", "https://mlflow.example.com")); err != nil || v != "env-secret" {
		t.Fatalf("env: %q %v", v, err)
	}
	if v, err := resolve(ctx, src(&evalsiv1alpha1.SourceCredentials{File: "mlflow-token/token"}, "studio", "https://anywhere.example.org")); err != nil || v != "file-secret" {
		t.Fatalf("file: %q %v", v, err)
	}
	// Another project, and the right project at the wrong host, are refused.
	for name, s := range map[string]*evalsiv1alpha1.TraceSource{
		"other project": src(&evalsiv1alpha1.SourceCredentials{Env: "STUDIO_MLFLOW_TOKEN"}, "rival", "https://mlflow.example.com"),
		"wrong host":    src(&evalsiv1alpha1.SourceCredentials{Env: "STUDIO_MLFLOW_TOKEN"}, "studio", "https://attacker.example.net"),
		"plain http":    src(&evalsiv1alpha1.SourceCredentials{Env: "STUDIO_MLFLOW_TOKEN"}, "studio", "http://mlflow.example.com"),
		"ungranted":     src(&evalsiv1alpha1.SourceCredentials{Env: "HOME"}, "studio", "https://mlflow.example.com"),
		"both":          src(&evalsiv1alpha1.SourceCredentials{Env: "A", File: "b/c"}, "studio", "https://mlflow.example.com"),
	} {
		if v, err := resolve(ctx, s); err == nil || v != "" {
			t.Errorf("%s: resolved %q, err %v", name, v, err)
		}
	}
	// A path outside the mount is not a file name.
	for _, bad := range []string{"../etc/passwd", "a/../b", "a/b/c", "/etc/passwd", "a"} {
		if _, ok, err := credentials.SourceUse(&evalsiv1alpha1.TraceSource{Credentials: &evalsiv1alpha1.SourceCredentials{File: bad}}); err == nil && ok {
			t.Errorf("file %q was accepted", bad)
		}
	}
	// A rotated Secret is read again; an unset variable and a missing file are errors.
	if err := os.WriteFile(file, []byte("rotated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if v, _ := resolve(ctx, src(&evalsiv1alpha1.SourceCredentials{File: "mlflow-token/token"}, "studio", "https://x.example.org")); v != "rotated" {
		t.Fatalf("not re-read: %q", v)
	}
	os.Unsetenv("STUDIO_MLFLOW_TOKEN")
	if _, err := resolve(ctx, src(&evalsiv1alpha1.SourceCredentials{Env: "STUDIO_MLFLOW_TOKEN"}, "studio", "https://mlflow.example.com")); err == nil {
		t.Fatal("an unset variable resolved")
	}
}

func newService(t *testing.T) (*Service, *harness) {
	t.Helper()
	h := newHarness(t, nil)
	svc := NewService(h.st, h.m, ServiceOptions{
		AllowHosts:   []string{"store"},
		PolicyExists: func(project, name string) bool { return project == "p" && name == "quality" },
		Now:          func() time.Time { return h.now },
	})
	return svc, h
}

func apply(t *testing.T, svc *Service, src *evalsiv1alpha1.TraceSource, validateOnly bool) (*evalsiv1alpha1.TraceSource, error) {
	t.Helper()
	resp, err := svc.ApplySource(context.Background(), connect.NewRequest(&evalsiv1alpha1.ApplySourceRequest{Source: src, ValidateOnly: validateOnly}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetSource(), nil
}

func TestApplySourceValidates(t *testing.T) {
	svc, h := newService(t)
	h.m.opts.Factories["mlflow"] = func(src *evalsiv1alpha1.TraceSource, _ string, _ *http.Client) (Connector, error) {
		if len(src.GetLocations()) == 0 {
			return nil, errTest("locations must list at least one experiment ID")
		}
		return h.fs, nil
	}
	good := func() *evalsiv1alpha1.TraceSource {
		return &evalsiv1alpha1.TraceSource{Name: "studio", Project: "p", Connector: "mlflow", Endpoint: "http://store:5000", Locations: []string{"1"}, Policies: []string{"quality"}}
	}
	for name, mutate := range map[string]func(*evalsiv1alpha1.TraceSource){
		"bad name":         func(s *evalsiv1alpha1.TraceSource) { s.Name = "Studio!" },
		"no connector":     func(s *evalsiv1alpha1.TraceSource) { s.Connector = "" },
		"unknown":          func(s *evalsiv1alpha1.TraceSource) { s.Connector = "splunk" },
		"loopback":         func(s *evalsiv1alpha1.TraceSource) { s.Endpoint = "http://127.0.0.1:5000" },
		"no locations":     func(s *evalsiv1alpha1.TraceSource) { s.Locations = nil },
		"bad override":     func(s *evalsiv1alpha1.TraceSource) { s.Overrides = map[string]string{"input": "trace.request"} },
		"override field":   func(s *evalsiv1alpha1.TraceSource) { s.Overrides = map[string]string{"usage": "'x'"} },
		"profile":          func(s *evalsiv1alpha1.TraceSource) { s.Profile = "weird" },
		"fast poll": func(s *evalsiv1alpha1.TraceSource) {
			s.Poll = &evalsiv1alpha1.Poll{Interval: durationpb.New(time.Millisecond)}
		},
		"short horizon":  func(s *evalsiv1alpha1.TraceSource) { s.MaxTraceDuration = durationpb.New(time.Second) },
		"no such policy": func(s *evalsiv1alpha1.TraceSource) { s.Policies = []string{"nope"} },
		"both credentials": func(s *evalsiv1alpha1.TraceSource) {
			s.Credentials = &evalsiv1alpha1.SourceCredentials{Env: "A", File: "b/c"}
		},
	} {
		src := good()
		mutate(src)
		_, err := apply(t, svc, src, false)
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("%s: got %v", name, err)
		}
	}
	// validate_only checks and stores nothing.
	if _, err := apply(t, svc, good(), true); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.st.ListSources(context.Background(), ""); len(got) != 1 { // only the harness's own source
		t.Fatalf("validate_only stored a source: %d", len(got))
	}
	out, err := apply(t, svc, good(), false)
	if err != nil || out.GetProject() != "p" || out.GetUpdatedAt() == nil {
		t.Fatalf("apply: %v %v", out, err)
	}
	// An empty project is the default project.
	d := good()
	d.Project, d.Name, d.Policies = "", "in-default", nil
	if out, err := apply(t, svc, d, false); err != nil || out.GetProject() != "default" {
		t.Fatalf("default project: %v %v", out, err)
	}
}

type errTest string

func (e errTest) Error() string { return string(e) }

func TestPauseResumeBackfillDelete(t *testing.T) {
	svc, h := newService(t)
	ctx := context.Background()
	key := &evalsiv1alpha1.GetSourceRequest{Project: "p", Name: "studio"}

	paused, err := svc.PauseSource(ctx, connect.NewRequest(&evalsiv1alpha1.PauseSourceRequest{Project: "p", Name: "studio"}))
	if err != nil || !paused.Msg.GetSource().GetPaused() || paused.Msg.GetSource().GetStatus().GetPhase() != evalsiv1alpha1.SourcePhase_SOURCE_PHASE_PAUSED {
		t.Fatalf("pause: %v %v", paused, err)
	}
	if got, _ := svc.GetSource(ctx, connect.NewRequest(key)); !got.Msg.GetSource().GetPaused() {
		t.Fatal("pause not stored")
	}
	resumed, err := svc.ResumeSource(ctx, connect.NewRequest(&evalsiv1alpha1.ResumeSourceRequest{Project: "p", Name: "studio"}))
	if err != nil || resumed.Msg.GetSource().GetPaused() {
		t.Fatalf("resume: %v %v", resumed, err)
	}

	// Backfill needs a start: pass one, or have set backfill.since.
	if _, err := svc.BackfillSource(ctx, connect.NewRequest(&evalsiv1alpha1.BackfillSourceRequest{Project: "p", Name: "studio"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("backfill without a start: %v", err)
	}
	if _, err := svc.BackfillSource(ctx, connect.NewRequest(&evalsiv1alpha1.BackfillSourceRequest{Project: "p", Name: "studio", Since: timestamppb.New(h.now.Add(time.Hour))})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("backfill from the future: %v", err)
	}
	since := h.now.Add(-6 * time.Hour)
	out, err := svc.BackfillSource(ctx, connect.NewRequest(&evalsiv1alpha1.BackfillSourceRequest{Project: "p", Name: "studio", Since: timestamppb.New(since)}))
	if err != nil || out.Msg.GetSource().GetStatus().GetPhase() != evalsiv1alpha1.SourcePhase_SOURCE_PHASE_BACKFILLING || !out.Msg.GetSource().GetStatus().GetWatermark().AsTime().Equal(since) {
		t.Fatalf("backfill: %v %v", out, err)
	}

	// A backfill requested while a cycle runs wins over that cycle's state.
	h.now = h.now.Add(48 * time.Hour)
	h.fs.set(info("tr-old", since.Add(time.Minute)))
	h.fs.pageSize = 1
	h.fs.set(info("tr-1", since.Add(time.Minute)), info("tr-2", since.Add(2*time.Minute)), info("tr-3", since.Add(3*time.Minute)))
	h.ing.block = nil
	reset := since.Add(-time.Hour)
	h.ing.onCall = func(n int) {
		if n == 1 { // after the first page
			st, _ := h.st.SourceState(ctx, "p", "studio")
			st.Watermark, st.BackfillFrom = reset, reset
			_ = h.st.PutSourceState(ctx, "p", "studio", st)
		}
	}
	if err := h.cycle(t); !errors.Is(err, errSuperseded) { // run() treats this as success
		t.Fatalf("got %v, want the cycle to be superseded", err)
	}
	if st := h.state(t); !st.Watermark.Equal(reset) || !st.BackfillFrom.Equal(reset) {
		t.Fatalf("the stale cycle overwrote the backfill reset: %+v", st)
	}

	if _, err := svc.DeleteSource(ctx, connect.NewRequest(&evalsiv1alpha1.DeleteSourceRequest{Project: "p", Name: "studio"})); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetSource(ctx, connect.NewRequest(key)); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("after delete: %v", err)
	}
	if _, err := svc.DeleteSource(ctx, connect.NewRequest(&evalsiv1alpha1.DeleteSourceRequest{Project: "p", Name: "studio"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("second delete: %v", err)
	}
}
