package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1/evalsiv1alpha1connect"
)

func TestRun(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantOut  string
		wantErr  string
	}{
		{name: "no args", args: nil, wantCode: 2, wantErr: "usage:"},
		{name: "version", args: []string{"version"}, wantCode: 0, wantOut: "api evalsi.v1alpha1"},
		{name: "serve bad config", args: []string{"serve", "--config", "/nonexistent/evalsi.yaml"}, wantCode: 1, wantErr: "no such file"},
		{name: "serve bad flag", args: []string{"serve", "--bogus"}, wantCode: 2, wantErr: "flag provided but not defined"},
		{name: "unknown", args: []string{"bogus"}, wantCode: 2, wantErr: `unknown command "bogus"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if got := run(context.Background(), tt.args, &stdout, &stderr); got != tt.wantCode {
				t.Fatalf("exit code = %d, want %d", got, tt.wantCode)
			}
			if !strings.Contains(stdout.String(), tt.wantOut) {
				t.Errorf("stdout = %q, want it to contain %q", stdout.String(), tt.wantOut)
			}
			if !strings.Contains(stderr.String(), tt.wantErr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.wantErr)
			}
		})
	}
}

// TestGeneratedAPI guards the generated code against silent drift from the
// names the rest of the system relies on.
func TestGeneratedAPI(t *testing.T) {
	if got, want := evalsiv1alpha1connect.EvaluationServiceName, "evalsi.v1alpha1.EvaluationService"; got != want {
		t.Errorf("service name = %q, want %q", got, want)
	}
	fields := (&evalsiv1alpha1.Record{}).ProtoReflect().Descriptor().Fields()
	for _, name := range []protoreflect.Name{"id", "input", "output", "reference", "context", "trajectory", "usage", "metadata", "provenance"} {
		if fields.ByName(name) == nil {
			t.Errorf("Record is missing field %q", name)
		}
	}
}

func TestAuthCheck(t *testing.T) {
	dir := t.TempDir()
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"auth", "new-key"}, &out, &errb); code != 0 {
		t.Fatalf("new-key: %d %s", code, errb.String())
	}
	var key, hash string
	for _, line := range strings.Split(out.String(), "\n") {
		if v, ok := strings.CutPrefix(line, "key:  "); ok {
			key = v
		}
		if v, ok := strings.CutPrefix(line, "hash: "); ok {
			hash = v
		}
	}
	cfg := filepath.Join(dir, "evalsi.yaml")
	body := "data_dir: " + filepath.Join(dir, "data") + `
auth:
  api_keys:
    keys: [{name: ci, key: "` + hash + `", roles: {support: [runner]}}]
rbac:
  projects: {support: {}}
authorization:
  rules: [{require: '!resource.runs_code'}]
`
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"--action", "runs.create", "--project", "support", "--resource", `{"runs_code":false}`}, 0, "role runner in support (API key ci)"},
		{[]string{"--action", "runs.create", "--project", "support", "--resource", `{"runs_code":true}`}, 3, "require rule 0 does not hold"},
		{[]string{"--action", "policies.write", "--project", "support", "--resource", `{"runs_code":false}`}, 3, "no role or rule grants"},
	} {
		out.Reset()
		args := append([]string{"auth", "check", "--config", cfg, "--api-key", key}, tc.args...)
		if code := run(context.Background(), args, &out, &errb); code != tc.code || !strings.Contains(out.String(), tc.want) {
			t.Errorf("%v: code %d, output:\n%s\nstderr: %s", tc.args, code, out.String(), errb.String())
		}
	}
	out.Reset()
	if code := run(context.Background(), []string{"auth", "check", "--config", cfg, "--api-key", "evk_wrong", "--action", "runs.read"}, &out, &errb); code != 3 || !strings.Contains(out.String(), "FAILED") {
		t.Errorf("bad key: %d %s", code, out.String())
	}
}
