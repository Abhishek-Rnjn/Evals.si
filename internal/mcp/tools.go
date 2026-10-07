package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"sigs.k8s.io/yaml"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1/evalsiv1alpha1connect"
)

type tool struct {
	name        string
	description string
	schema      map[string]any
	readOnly    bool
	call        func(ctx context.Context, c *http.Client, args map[string]any) (map[string]any, error)
}

func (t tool) describe() map[string]any {
	return map[string]any{
		"name":        t.name,
		"description": t.description,
		"inputSchema": t.schema,
		"annotations": map[string]any{"readOnlyHint": t.readOnly, "openWorldHint": false},
	}
}

func object(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

var str = map[string]any{"type": "string"}

func (s *Server) toolset() []tool {
	evaluatorRef := map[string]any{"oneOf": []any{
		map[string]any{"type": "string", "description": "an evaluator reference, e.g. exact-match"},
		object(map[string]any{"ref": str, "name": str, "params": map[string]any{"type": "object"}}, "ref"),
	}}
	return []tool{
		{
			name:        "list_evaluators",
			description: "List the evaluators this server can run, by pack, and the judges configured.",
			schema:      object(map[string]any{"pack": map[string]any{"type": "string", "description": "only this pack"}}),
			readOnly:    true,
			call:        listEvaluators,
		},
		{
			name: "evaluate",
			description: "Score records (input, output, reference, metadata) with evaluators. " +
				"Returns each metric's mean and confidence interval, and per-record scores.",
			schema: object(map[string]any{
				"records":    map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
				"evaluators": map[string]any{"type": "array", "items": evaluatorRef, "minItems": 1},
				"project":    str,
				"judge":      map[string]any{"type": "string", "description": "a judge configured on the server"},
			}, "records", "evaluators"),
			readOnly: true,
			call:     evaluate,
		},
		{
			name: "run",
			description: "Start a run from a run spec (the EvalRun document, or just its spec), wait for " +
				"it, and compare it with a baseline: by default the previous finished run of the same " +
				"name in the project. Reports summaries, gates and significant changes.",
			schema: object(map[string]any{
				"spec": map[string]any{
					"description": "the run spec: an object, or YAML/JSON text (an EvalRun document or its spec)",
				},
				"name":     str,
				"project":  str,
				"labels":   map[string]any{"type": "object", "additionalProperties": str},
				"wait":     map[string]any{"type": "boolean", "description": "wait for the run (default true)"},
				"baseline": map[string]any{"type": "string", "description": "'previous' (default), 'none', or a run id"},
			}, "spec"),
			call: s.run,
		},
		{
			name:        "get_run",
			description: "A run's status, summaries and gates.",
			schema:      object(map[string]any{"run_id": str}, "run_id"),
			readOnly:    true,
			call:        getRun,
		},
		{
			name:        "compare_runs",
			description: "Paired comparison of two runs, record by record.",
			schema:      object(map[string]any{"baseline": str, "candidate": str}, "baseline", "candidate"),
			readOnly:    true,
			call:        compareRuns,
		},
	}
}

func result(text string, structured proto.Message) (map[string]any, error) {
	out := map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": false,
	}
	if structured != nil {
		raw, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(structured)
		if err != nil {
			return nil, err
		}
		var v map[string]any
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, err
		}
		out["structuredContent"] = v
	}
	return out, nil
}

// rpcErr makes a Connect error readable for the model.
func rpcErr(err error) error {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return fmt.Errorf("%s: %s", ce.Code(), ce.Message())
	}
	return err
}

func stringArg(args map[string]any, key string) string {
	v, _ := args[key].(string)
	return v
}

func listEvaluators(ctx context.Context, c *http.Client, args map[string]any) (map[string]any, error) {
	client := evalsiv1alpha1connect.NewCatalogServiceClient(c, internalURL)
	resp, err := client.ListEvaluators(ctx, connect.NewRequest(&evalsiv1alpha1.ListEvaluatorsRequest{}))
	if err != nil {
		return nil, rpcErr(err)
	}
	pack := stringArg(args, "pack")
	msg := resp.Msg
	if pack != "" {
		var kept []*evalsiv1alpha1.EvaluatorManifest
		for _, e := range msg.GetEvaluators() {
			if e.GetPack() == pack {
				kept = append(kept, e)
			}
		}
		if len(kept) == 0 {
			return nil, fmt.Errorf("no evaluators in pack %q", pack)
		}
		msg = &evalsiv1alpha1.ListEvaluatorsResponse{Evaluators: kept, Judges: msg.GetJudges(), DefaultJudge: msg.GetDefaultJudge()}
	}
	byPack := map[string][]string{}
	for _, e := range msg.GetEvaluators() {
		line := e.GetName()
		if e.GetRequires().GetJudge() {
			line += " (needs a judge)"
		}
		if e.GetDescription() != "" {
			line += ": " + e.GetDescription()
		}
		byPack[e.GetPack()] = append(byPack[e.GetPack()], line)
	}
	packs := make([]string, 0, len(byPack))
	for p := range byPack {
		packs = append(packs, p)
	}
	sort.Strings(packs)
	var b strings.Builder
	for _, p := range packs {
		fmt.Fprintf(&b, "%s:\n", p)
		for _, l := range byPack[p] {
			fmt.Fprintf(&b, "  %s\n", l)
		}
	}
	if len(msg.GetJudges()) > 0 {
		fmt.Fprintf(&b, "judges: %s (default %s)\n", strings.Join(msg.GetJudges(), ", "), msg.GetDefaultJudge())
	}
	return result(b.String(), msg)
}

// contentize turns a string field into a Content ({"text": ...}).
func contentize(rec map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range rec {
		switch k {
		case "input", "output", "reference":
			if s, ok := v.(string); ok {
				v = map[string]any{"text": s}
			}
		}
		out[k] = v
	}
	return out
}

func unmarshalInto(v any, m proto.Message) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return protojson.Unmarshal(raw, m)
}

func evaluate(ctx context.Context, c *http.Client, args map[string]any) (map[string]any, error) {
	records, _ := args["records"].([]any)
	evaluators, _ := args["evaluators"].([]any)
	if len(records) == 0 || len(evaluators) == 0 {
		return nil, errors.New("records and evaluators must be non-empty arrays")
	}
	req := &evalsiv1alpha1.EvaluateRequest{Project: stringArg(args, "project"), Judge: stringArg(args, "judge")}
	for i, r := range records {
		m, ok := r.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("record %d is not an object", i)
		}
		m = contentize(m)
		if _, ok := m["id"]; !ok {
			m["id"] = fmt.Sprint(i)
		}
		rec := &evalsiv1alpha1.Record{}
		if err := unmarshalInto(m, rec); err != nil {
			return nil, fmt.Errorf("record %d: %w", i, err)
		}
		req.Records = append(req.Records, rec)
	}
	for i, e := range evaluators {
		ref := &evalsiv1alpha1.EvaluatorRef{}
		switch v := e.(type) {
		case string:
			ref.Ref = v
		case map[string]any:
			if err := unmarshalInto(v, ref); err != nil {
				return nil, fmt.Errorf("evaluator %d: %w", i, err)
			}
		default:
			return nil, fmt.Errorf("evaluator %d: use a reference or {ref, params}", i)
		}
		req.Evaluators = append(req.Evaluators, ref)
	}
	client := evalsiv1alpha1connect.NewEvaluationServiceClient(c, internalURL)
	resp, err := client.Evaluate(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, rpcErr(err)
	}
	text := summaryText(resp.Msg.GetSummaries(), nil) + "\n\n" + recordRows(resp.Msg.GetResults())
	return result(text, resp.Msg)
}

func (s *Server) run(ctx context.Context, c *http.Client, args map[string]any) (map[string]any, error) {
	spec, name, project, labels, err := parseRunArgs(args)
	if err != nil {
		return nil, err
	}
	runs := evalsiv1alpha1connect.NewRunServiceClient(c, internalURL)
	baseline := stringArg(args, "baseline")
	if baseline == "" {
		baseline = "previous"
	}
	var baseID string
	switch baseline {
	case "none":
	case "previous":
		if baseID, err = previousRun(ctx, runs, name, project); err != nil {
			return nil, err
		}
	default:
		baseID = baseline
	}
	created, err := runs.CreateRun(ctx, connect.NewRequest(&evalsiv1alpha1.CreateRunRequest{Name: name, Project: project, Spec: spec, Labels: labels}))
	if err != nil {
		return nil, rpcErr(err)
	}
	run := created.Msg.GetRun()
	if wait, ok := args["wait"].(bool); ok && !wait {
		return result(fmt.Sprintf("run %s started (%s); poll it with get_run", run.GetId(), statusName(run.GetStatus())), run)
	}
	deadline := s.now().Add(s.waitFor())
	for !finished(run.GetStatus()) {
		if s.now().After(deadline) {
			return result(fmt.Sprintf("run %s is still %s after %s; poll it with get_run", run.GetId(), statusName(run.GetStatus()), s.waitFor()), run)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
		got, err := runs.GetRun(ctx, connect.NewRequest(&evalsiv1alpha1.GetRunRequest{Id: run.GetId()}))
		if err != nil {
			return nil, rpcErr(err)
		}
		run = got.Msg.GetRun()
	}
	text := fmt.Sprintf("run %s: %s\n", run.GetId(), statusName(run.GetStatus())) + summaryText(run.GetSummaries(), run.GetGates())
	if run.GetError() != "" {
		text += "\nerror: " + run.GetError()
	}
	out, err := result(text, run)
	if err != nil {
		return nil, err
	}
	if baseID != "" {
		cmp, err := runs.CompareRuns(ctx, connect.NewRequest(&evalsiv1alpha1.CompareRunsRequest{BaselineRunId: baseID, CandidateRunId: run.GetId()}))
		if err != nil {
			text += fmt.Sprintf("\n\ncould not compare with run %s: %v", baseID, rpcErr(err))
		} else {
			text += fmt.Sprintf("\n\ncompared with run %s:\n%s", baseID, comparisonText(cmp.Msg.GetComparisons()))
			raw, _ := protojson.MarshalOptions{UseProtoNames: true}.Marshal(cmp.Msg)
			var v map[string]any
			_ = json.Unmarshal(raw, &v)
			out["structuredContent"].(map[string]any)["comparison"] = map[string]any{"baseline_run_id": baseID, "comparisons": v["comparisons"]}
		}
		out["content"] = []map[string]any{{"type": "text", "text": text}}
	}
	return out, nil
}

// parseRunArgs reads the spec (an object, or YAML/JSON text; an EvalRun
// document or its spec) and the run's name, project and labels.
func parseRunArgs(args map[string]any) (*evalsiv1alpha1.RunSpec, string, string, map[string]string, error) {
	var doc map[string]any
	switch v := args["spec"].(type) {
	case map[string]any:
		doc = v
	case string:
		raw, err := yaml.YAMLToJSON([]byte(v))
		if err != nil {
			return nil, "", "", nil, fmt.Errorf("spec: %w", err)
		}
		if err := json.Unmarshal(raw, &doc); err != nil || doc == nil {
			return nil, "", "", nil, errors.New("spec: must be an object")
		}
	default:
		return nil, "", "", nil, errors.New("spec is required")
	}
	name, project := stringArg(args, "name"), stringArg(args, "project")
	labels := map[string]string{}
	if inner, ok := doc["spec"].(map[string]any); ok && doc["kind"] != nil {
		if meta, ok := doc["metadata"].(map[string]any); ok {
			if name == "" {
				name, _ = meta["name"].(string)
			}
			if project == "" {
				project, _ = meta["project"].(string)
			}
			if l, ok := meta["labels"].(map[string]any); ok {
				for k, v := range l {
					labels[k] = fmt.Sprint(v)
				}
			}
		}
		doc = inner
	}
	if l, ok := args["labels"].(map[string]any); ok {
		for k, v := range l {
			labels[k] = fmt.Sprint(v)
		}
	}
	spec := &evalsiv1alpha1.RunSpec{}
	if err := unmarshalInto(doc, spec); err != nil {
		return nil, "", "", nil, fmt.Errorf("spec: %w", err)
	}
	return spec, name, project, labels, nil
}

func previousRun(ctx context.Context, runs evalsiv1alpha1connect.RunServiceClient, name, project string) (string, error) {
	if name == "" {
		return "", nil
	}
	token := ""
	for range 5 {
		resp, err := runs.ListRuns(ctx, connect.NewRequest(&evalsiv1alpha1.ListRunsRequest{Project: project, PageSize: 100, PageToken: token}))
		if err != nil {
			return "", rpcErr(err)
		}
		for _, r := range resp.Msg.GetRuns() {
			if r.GetName() == name && (r.GetStatus() == evalsiv1alpha1.RunStatus_RUN_STATUS_SUCCEEDED || r.GetStatus() == evalsiv1alpha1.RunStatus_RUN_STATUS_FAILED) {
				return r.GetId(), nil
			}
		}
		if token = resp.Msg.GetNextPageToken(); token == "" {
			break
		}
	}
	return "", nil
}

func getRun(ctx context.Context, c *http.Client, args map[string]any) (map[string]any, error) {
	id := stringArg(args, "run_id")
	if id == "" {
		return nil, errors.New("run_id is required")
	}
	client := evalsiv1alpha1connect.NewRunServiceClient(c, internalURL)
	resp, err := client.GetRun(ctx, connect.NewRequest(&evalsiv1alpha1.GetRunRequest{Id: id}))
	if err != nil {
		return nil, rpcErr(err)
	}
	run := resp.Msg.GetRun()
	text := fmt.Sprintf("run %s (%s): %s", run.GetId(), run.GetName(), statusName(run.GetStatus()))
	if p := run.GetProgress(); p != nil && !finished(run.GetStatus()) {
		text += fmt.Sprintf(" %d/%d", p.GetDone(), p.GetTotal())
	}
	text += "\n" + summaryText(run.GetSummaries(), run.GetGates())
	return result(text, run)
}

func compareRuns(ctx context.Context, c *http.Client, args map[string]any) (map[string]any, error) {
	a, b := stringArg(args, "baseline"), stringArg(args, "candidate")
	if a == "" || b == "" {
		return nil, errors.New("baseline and candidate are required")
	}
	client := evalsiv1alpha1connect.NewRunServiceClient(c, internalURL)
	resp, err := client.CompareRuns(ctx, connect.NewRequest(&evalsiv1alpha1.CompareRunsRequest{BaselineRunId: a, CandidateRunId: b}))
	if err != nil {
		return nil, rpcErr(err)
	}
	return result(comparisonText(resp.Msg.GetComparisons()), resp.Msg)
}

// --- text for the model ------------------------------------------------------

func finished(s evalsiv1alpha1.RunStatus) bool {
	switch s {
	case evalsiv1alpha1.RunStatus_RUN_STATUS_SUCCEEDED, evalsiv1alpha1.RunStatus_RUN_STATUS_FAILED,
		evalsiv1alpha1.RunStatus_RUN_STATUS_ERROR, evalsiv1alpha1.RunStatus_RUN_STATUS_CANCELLED:
		return true
	}
	return false
}

func statusName(s evalsiv1alpha1.RunStatus) string {
	return strings.ToLower(strings.TrimPrefix(s.String(), "RUN_STATUS_"))
}

func f3(v *float64) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf("%.3f", *v)
}

func summaryText(sums []*evalsiv1alpha1.MetricSummary, gates []*evalsiv1alpha1.GateResult) string {
	var b strings.Builder
	b.WriteString("metric  n  mean  [CI]")
	for _, s := range sums {
		fmt.Fprintf(&b, "\n%s  %d  %s", s.GetMetric(), s.GetN(), f3(s.Mean))
		if ci := s.GetCi(); ci != nil {
			fmt.Fprintf(&b, "  [%.3f, %.3f]", ci.GetLow(), ci.GetHigh())
		}
		if s.GetErrors() > 0 {
			fmt.Fprintf(&b, "  errors=%d", s.GetErrors())
		}
		if s.GetSkipped() > 0 {
			fmt.Fprintf(&b, "  skipped=%d", s.GetSkipped())
		}
	}
	for _, g := range gates {
		verdict := "passed"
		if !g.GetPassed() {
			verdict = "FAILED"
		}
		fmt.Fprintf(&b, "\ngate %s: %s", g.GetGate().GetMetric(), verdict)
		if g.Value != nil {
			fmt.Fprintf(&b, " (%.3f)", g.GetValue())
		}
		if g.GetReason() != "" {
			fmt.Fprintf(&b, " %s", g.GetReason())
		}
	}
	return b.String()
}

const maxRows = 50

func recordRows(results []*evalsiv1alpha1.EvaluationResult) string {
	var b strings.Builder
	for i, r := range results {
		if i == maxRows {
			fmt.Fprintf(&b, "... %d more results\n", len(results)-maxRows)
			break
		}
		if r.GetOutcome() != evalsiv1alpha1.Outcome_OUTCOME_SCORED {
			fmt.Fprintf(&b, "%s %s: %s %s\n", r.GetRecordId(), r.GetEvaluator(), strings.ToLower(strings.TrimPrefix(r.GetOutcome().String(), "OUTCOME_")), r.GetReason())
			continue
		}
		for _, sc := range r.GetScores() {
			var v string
			switch x := sc.GetValue().(type) {
			case *evalsiv1alpha1.Score_Number:
				v = fmt.Sprintf("%.3f", x.Number)
			case *evalsiv1alpha1.Score_Passed:
				v = fmt.Sprint(x.Passed)
			case *evalsiv1alpha1.Score_Label:
				v = x.Label
			}
			name := sc.GetName()
			if name == "" {
				name = r.GetEvaluator()
			}
			fmt.Fprintf(&b, "%s %s: %s", r.GetRecordId(), name, v)
			if sc.GetExplanation() != "" {
				fmt.Fprintf(&b, " (%s)", sc.GetExplanation())
			}
			b.WriteString("\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func comparisonText(cs []*evalsiv1alpha1.MetricComparison) string {
	if len(cs) == 0 {
		return "no shared metrics"
	}
	var b strings.Builder
	for i, c := range cs {
		if i > 0 {
			b.WriteString("\n")
		}
		verdict := "no significant change"
		if c.GetSignificant() {
			if c.GetDiff() < 0 {
				verdict = "significantly lower"
			} else {
				verdict = "significantly higher"
			}
		}
		fmt.Fprintf(&b, "%s: %s -> %s (diff %s", c.GetMetric(), f3(c.BaselineMean), f3(c.CandidateMean), f3(c.Diff))
		if ci := c.GetDiffCi(); ci != nil {
			fmt.Fprintf(&b, " [%.3f, %.3f]", ci.GetLow(), ci.GetHigh())
		}
		fmt.Fprintf(&b, ", n=%d): %s", c.GetPairedN(), verdict)
	}
	return b.String()
}
