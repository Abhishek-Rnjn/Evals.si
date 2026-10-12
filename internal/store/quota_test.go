package store

import (
	"context"
	"path/filepath"
	"testing"
)

// Sandbox time is kept per project and day beside the token counts, and adds
// up across calls.
func TestSandboxUsageAccumulatesPerProjectAndDay(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "db", "evalsi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if u, err := st.AddUsage(ctx, "p", "2026-10-12", Usage{JudgeTokens: 5, SandboxMillis: 1500}); err != nil || u.SandboxMillis != 1500 || u.JudgeTokens != 5 {
		t.Fatalf("first: %+v %v", u, err)
	}
	if u, err := st.AddUsage(ctx, "p", "2026-10-12", Usage{SandboxMillis: 2500}); err != nil || u.SandboxMillis != 4000 || u.JudgeTokens != 5 {
		t.Fatalf("second: %+v %v", u, err)
	}
	// Tokens alone leave the sandbox count alone.
	if u, err := st.AddUsage(ctx, "p", "2026-10-12", Usage{TargetTokens: 7}); err != nil || u.SandboxMillis != 4000 || u.TargetTokens != 7 {
		t.Fatalf("tokens only: %+v %v", u, err)
	}
	for _, other := range [][2]string{{"q", "2026-10-12"}, {"p", "2026-10-13"}} {
		if u, err := st.UsageOn(ctx, other[0], other[1]); err != nil || u != (Usage{}) {
			t.Errorf("%v: %+v %v", other, u, err)
		}
	}
}
