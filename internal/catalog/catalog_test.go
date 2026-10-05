package catalog

import (
	"strings"
	"testing"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

func TestResolve(t *testing.T) {
	c := New([]*evalsiv1alpha1.EvaluatorManifest{
		{Name: "builtin/exact-match", Version: "1.0.0"},
		{Name: "builtin/length", Version: "1.0.0"},
		{Name: "acme/length", Version: "2.0.0"},
	})
	for ref, want := range map[string]string{
		"exact-match":               "builtin/exact-match",
		"builtin/exact-match@1.0.0": "builtin/exact-match",
		"acme/length":               "acme/length",
	} {
		m, err := c.Resolve(ref)
		if err != nil || m.GetName() != want {
			t.Errorf("Resolve(%q) = %v, %v; want %s", ref, m.GetName(), err, want)
		}
	}
	for ref, want := range map[string]string{
		"length":                    "ambiguous",
		"nope":                      "unknown evaluator",
		"builtin/exact-match@9.0.0": "pinned",
	} {
		if _, err := c.Resolve(ref); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Resolve(%q) error = %v, want %q", ref, err, want)
		}
	}
	if got := len(c.Manifests()); got != 3 {
		t.Errorf("Manifests() has %d entries", got)
	}
}
