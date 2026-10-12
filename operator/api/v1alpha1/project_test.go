package v1alpha1

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestProjectPolicy(t *testing.T) {
	policy := ProjectPolicy{Home: "evalsi", Namespaces: map[string][]string{"support": {"team-a"}, "shared": {"*"}}}
	for _, tc := range []struct {
		namespace, label, want string
	}{
		{"team-a", "", "team-a"},
		{"team-a", "team-a", "team-a"},
		{"team-a", "support", "support"},
		{"team-b", "shared", "shared"},
		{"evalsi", "checkout", "checkout"},
		{"team-a", "checkout", ""},
		{"team-b", "support", ""},
	} {
		obj := &metav1.ObjectMeta{Namespace: tc.namespace}
		if tc.label != "" {
			obj.Labels = map[string]string{ProjectLabel: tc.label}
		}
		got, err := policy.Project(obj)
		if got != tc.want || (err != nil) != (tc.want == "") {
			t.Errorf("%s with label %q: %q, %v; want %q", tc.namespace, tc.label, got, err, tc.want)
		}
	}
	// Without a home namespace, the label only maps where configured.
	if _, err := (ProjectPolicy{}).Project(&metav1.ObjectMeta{Namespace: "evalsi", Labels: map[string]string{ProjectLabel: "x"}}); err == nil {
		t.Error("an empty policy allowed a foreign project")
	}
}

func TestParseProjectNamespaces(t *testing.T) {
	got, err := ParseProjectNamespaces([]string{"support=team-a, team-b", "shared=*"})
	if err != nil || len(got["support"]) != 2 || got["support"][1] != "team-b" || got["shared"][0] != "*" {
		t.Fatalf("%v %v", got, err)
	}
	for _, bad := range []string{"support", "=team-a", "support="} {
		if _, err := ParseProjectNamespaces([]string{bad}); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}
