package server

import (
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/config"
)

// TestExamplePoliciesParse keeps the documented example config usable.
func TestExamplePoliciesParse(t *testing.T) {
	cfg, err := config.Load("../../examples/server/evalsi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Policies) == 0 {
		t.Fatal("example config has no policies")
	}
	for i, raw := range cfg.Policies {
		p := &evalsiv1alpha1.OnlineEvalPolicy{}
		if err := protojson.Unmarshal(raw, p); err != nil {
			t.Errorf("policies[%d]: %v", i, err)
		}
	}
}
