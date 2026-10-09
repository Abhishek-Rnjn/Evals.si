package controllers

import "testing"

func TestChildNamesStartWithThePrefix(t *testing.T) {
	for _, c := range []struct{ prefix, want string }{
		{"", "evalsi-evaluator-judges"},
		{"evalsi", "evalsi-evaluator-judges"},
		{"team-a-evalsi", "team-a-evalsi-evaluator-judges"},
	} {
		if got := childName(c.prefix, "evaluator", "judges"); got != c.want {
			t.Errorf("prefix %q: %s, want %s", c.prefix, got, c.want)
		}
	}
}
