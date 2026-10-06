package e2e

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abhishek-rnjn/evals.si/internal/sandbox"
)

// rewardScript scores the same rollouts in-process and through the server's
// Reward Service, with the evalsi library a trainer would use, and prints
// both sets of totals.
const rewardScript = `
import json, sys
import evalsi.rewards as rewards

spec = {
    "name": "code-grpo",
    "components": [
        {"ref": "format-check", "weight": 0.1, "gate": True,
         "params": {"pattern": "(?s).*` + "```" + `python.*` + "```" + `.*"}},
        {"ref": "code-exec-tests", "weight": 0.9, "sandbox": {"minIsolation": "confined"}},
    ],
}
tests = [{"input": "2 3", "output": "5"}, {"input": "-10 4", "output": "-6"}, {"input": "0 0", "output": "0"}]
fence = "` + "```" + `"
completions = [
    fence + "python\na, b = map(int, input().split())\nprint(a + b)\n" + fence,
    fence + "python\na, b = map(int, input().split())\nprint(abs(a + b))\n" + fence,
    "a, b = map(int, input().split())\nprint(a + b)\n",
    fence + "python\nimport urllib.request\nprint(urllib.request.urlopen('http://example.com').status)\n" + fence,
    fence + "python\nopen('/usr/evalsi-escape', 'w')\n" + fence,
]
kwargs = {"prompts": ["add two ints"] * len(completions), "completions": completions,
          "tests": [tests] * len(completions)}
local = rewards.load(spec)(**kwargs)
remote = rewards.load(spec, server=sys.argv[1], batch_size=2)
first = remote(**kwargs)
again = remote(**kwargs)
cached = all(c.cached for r in remote.last_results for c in r.components.values())
print(json.dumps({"local": local, "remote": first, "again": again, "cached": cached,
                  "breakdown": [r.to_dict() for r in remote.last_results]}))
`

// TestRewards: the server's Reward Service and the in-process library give
// the same rewards for code rollouts run in the real sandbox, and repeats
// come from the cache.
func TestRewards(t *testing.T) {
	e := start(t)
	probe, err := sandbox.New(sandbox.Config{})
	if err != nil {
		t.Fatal(err)
	}
	usable := false
	for _, st := range probe.Probe(context.Background()) {
		usable = usable || st.Available
	}
	if !usable {
		t.Skip("no sandbox rung works on this host")
	}
	script := filepath.Join(t.TempDir(), "rewards.py")
	if err := os.WriteFile(script, []byte(rewardScript), 0o600); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(e.workerCmd[0], script, e.base)
	cmd.Env = append(os.Environ(), "EVALSID="+self)
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("reward script: %v\n%s", err, stderr)
	}
	var got struct {
		Local, Remote, Again []float64
		Cached               bool
		Breakdown            []map[string]any
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	// Correct; wrong on one case (abs); unfenced (format gate fails);
	// network denied; host write denied.
	want := []float64{1.0, 0.1 + 0.9*2/3, 0, 0.1, 0.1}
	for i, w := range want {
		for name, vals := range map[string][]float64{"local": got.Local, "remote": got.Remote, "again": got.Again} {
			if len(vals) != len(want) || abs(vals[i]-w) > 1e-9 {
				t.Errorf("%s rollout %d: got %v, want %v (breakdown %v)", name, i, vals, want, got.Breakdown)
				break
			}
		}
	}
	if !got.Cached {
		t.Errorf("repeated rollouts were not served from the cache: %v", got.Breakdown)
	}
	if _, err := os.Stat("/usr/evalsi-escape"); err == nil {
		os.Remove("/usr/evalsi-escape")
		t.Fatal("a rollout wrote to the host")
	}

	resp, err := h2cClient().Get(e.base + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "evalsi_reward_rollouts_total 10") {
		t.Errorf("reward metrics missing or wrong:\n%s", body)
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
