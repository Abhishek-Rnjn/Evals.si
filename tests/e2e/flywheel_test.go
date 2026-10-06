package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1/evalsiv1alpha1connect"
)

// TestFlywheel: production traces arrive over OTLP; `evalsi shadow` replays
// their inputs against a candidate model and scores the recorded answers the
// same way; `evalsi promote` turns the candidate's misses into a dataset that
// a later run loads.
func TestFlywheel(t *testing.T) {
	flywheel(t, start(t))
}

func flywheel(t *testing.T, e env) {
	ctx := context.Background()
	attr := func(k, v string) map[string]any {
		return map[string]any{"key": k, "value": map[string]any{"stringValue": v}}
	}
	msgs := func(role, text string) string {
		raw, _ := json.Marshal([]any{map[string]any{"role": role, "parts": []any{map[string]any{"type": "text", "content": text}}}})
		return string(raw)
	}
	recorded := map[string]string{"France": "Paris", "Italy": "Milan", "Japan": "Kyoto"}
	i := 0
	for country, answer := range recorded {
		i++
		payload, _ := json.Marshal(map[string]any{"resourceSpans": []any{map[string]any{
			"resource": map[string]any{"attributes": []any{attr("service.name", "qa-bot")}},
			"scopeSpans": []any{map[string]any{"spans": []any{map[string]any{
				"traceId": fmt.Sprintf("%032x", 100+i), "spanId": fmt.Sprintf("%016x", 100+i), "name": "chat",
				"startTimeUnixNano": fmt.Sprint(time.Now().UnixNano()), "endTimeUnixNano": fmt.Sprint(time.Now().UnixNano() + 1000),
				"attributes": []any{
					attr("gen_ai.operation.name", "chat"),
					attr("gen_ai.input.messages", msgs("user", "Capital of "+country+"?")),
					attr("gen_ai.output.messages", msgs("assistant", answer)),
				},
			}}}},
		}}})
		resp, err := http.Post(e.base+"/v1/traces", "application/json", bytes.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	traces := evalsiv1alpha1connect.NewTraceServiceClient(h2cClient(), e.base, connect.WithGRPC())
	deadline := time.Now().Add(20 * time.Second)
	for {
		list, err := traces.ListTraces(ctx, connect.NewRequest(&evalsiv1alpha1.ListTracesRequest{Service: "qa-bot"}))
		if err == nil && len(list.Msg.GetTraces()) == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("traces not stored: %v %v", list, err)
		}
		time.Sleep(100 * time.Millisecond)
	}

	cli := func(args ...string) []byte {
		t.Helper()
		full := append(append([]string{}, e.workerCmd[1:]...), args...)
		out, err := exec.Command(e.workerCmd[0], full...).Output()
		if err != nil {
			var stderr []byte
			if ee, ok := err.(*exec.ExitError); ok {
				stderr = ee.Stderr
			}
			t.Fatalf("evalsi %v: %v\n%s%s", args, err, out, stderr)
		}
		return out
	}
	spec := filepath.Join(t.TempDir(), "candidate.yaml")
	doc := fmt.Sprintf(`apiVersion: evals.si/v1alpha1
kind: EvalRun
metadata: {name: qa-candidate}
spec:
  target: {connector: openai-compatible, model: fake-model, base_url: "%s"}
  dataset: {traces: {service: qa-bot, lookback: 1h}}
  evaluators: [{ref: contains, params: {substring: Rome}}]
`, e.model)
	if err := os.WriteFile(spec, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	var shadow struct {
		Baseline, Candidate struct {
			ID string `json:"id"`
		}
		Comparisons []struct {
			Metric        string  `json:"metric"`
			BaselineMean  float64 `json:"baselineMean"`
			CandidateMean float64 `json:"candidateMean"`
			PairedN       string  `json:"pairedN"`
		}
	}
	if err := json.Unmarshal(cli("shadow", "-f", spec, "--server", e.base, "--format", "json", "--quiet"), &shadow); err != nil {
		t.Fatal(err)
	}
	if len(shadow.Comparisons) != 1 || shadow.Comparisons[0].PairedN != "3" || shadow.Comparisons[0].BaselineMean != 0 ||
		shadow.Comparisons[0].CandidateMean < 0.33 || shadow.Comparisons[0].CandidateMean > 0.34 {
		t.Fatalf("shadow comparison %+v", shadow.Comparisons)
	}

	out := cli("promote", shadow.Candidate.ID, "--dataset", "rome-misses", "--when", `scores["contains"] < 1.0`, "--server", e.base)
	if !strings.Contains(string(out), "promoted 2 record(s) to promoted/default/rome-misses.jsonl") {
		t.Fatalf("promote: %s", out)
	}
	runs := evalsiv1alpha1connect.NewRunServiceClient(h2cClient(), e.base, connect.WithGRPC())
	created, err := runs.CreateRun(ctx, connect.NewRequest(&evalsiv1alpha1.CreateRunRequest{Spec: &evalsiv1alpha1.RunSpec{
		Target:     &evalsiv1alpha1.Target{Connector: "openai-compatible", Model: "fake-model", BaseUrl: e.model},
		Dataset:    &evalsiv1alpha1.DatasetSource{Source: &evalsiv1alpha1.DatasetSource_Path{Path: "promoted/default/rome-misses.jsonl"}},
		Evaluators: []*evalsiv1alpha1.EvaluatorRef{{Ref: "contains", Params: mustStruct(map[string]any{"substring": "Rome"})}},
	}}))
	if err != nil {
		t.Fatal(err)
	}
	if created.Msg.GetRun().GetRecords() != 2 {
		t.Errorf("the promoted dataset has %d records", created.Msg.GetRun().GetRecords())
	}
}
