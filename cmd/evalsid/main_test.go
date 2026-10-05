package main

import (
	"bytes"
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
		{name: "serve not yet", args: []string{"serve"}, wantCode: 1, wantErr: "Phase 1"},
		{name: "unknown", args: []string{"bogus"}, wantCode: 2, wantErr: `unknown command "bogus"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if got := run(tt.args, &stdout, &stderr); got != tt.wantCode {
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
