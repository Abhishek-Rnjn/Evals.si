package rewards

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/structpb"

	harnessv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/harness/v1alpha1"
	pluginv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/plugin/v1alpha1"
	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/catalog"
	"github.com/abhishek-rnjn/evals.si/internal/config"
	"github.com/abhishek-rnjn/evals.si/internal/evaluation"
)

type vectorCase struct {
	Name string `json:"name"`
	Spec struct {
		Floor      float64   `json:"floor"`
		OnError    string    `json:"onError"`
		Clip       []float64 `json:"clip"`
		Components []struct {
			Ref       string   `json:"ref"`
			Name      string   `json:"name"`
			Weight    *float64 `json:"weight"`
			Gate      bool     `json:"gate"`
			Threshold *float64 `json:"threshold"`
		} `json:"components"`
	} `json:"spec"`
	Components map[string]struct {
		Status string   `json:"status"`
		Value  *float64 `json:"value"`
		Reason string   `json:"reason"`
	} `json:"components"`
	Want struct {
		Total *float64 `json:"total"`
		Gated bool     `json:"gated"`
		Error bool     `json:"error"`
	} `json:"want"`
}

// TestComposeMatchesSharedVectors checks Compose against the cases the
// Python library's compose is checked against.
func TestComposeMatchesSharedVectors(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/reward_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Cases []vectorCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	onError := map[string]evalsiv1alpha1.RewardOnError{
		"": evalsiv1alpha1.RewardOnError_REWARD_ON_ERROR_UNSPECIFIED, "raise": evalsiv1alpha1.RewardOnError_REWARD_ON_ERROR_RAISE,
		"zero": evalsiv1alpha1.RewardOnError_REWARD_ON_ERROR_ZERO, "none": evalsiv1alpha1.RewardOnError_REWARD_ON_ERROR_NONE,
	}
	status := map[string]evalsiv1alpha1.RewardComponentStatus{
		"scored":  evalsiv1alpha1.RewardComponentStatus_REWARD_COMPONENT_STATUS_SCORED,
		"skipped": evalsiv1alpha1.RewardComponentStatus_REWARD_COMPONENT_STATUS_SKIPPED,
		"error":   evalsiv1alpha1.RewardComponentStatus_REWARD_COMPONENT_STATUS_ERROR,
	}
	for _, tc := range file.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			spec := &evalsiv1alpha1.RewardSpec{Floor: tc.Spec.Floor, OnError: onError[tc.Spec.OnError]}
			if len(tc.Spec.Clip) == 2 {
				spec.Clip = &evalsiv1alpha1.RewardClip{Low: tc.Spec.Clip[0], High: tc.Spec.Clip[1]}
			}
			var keys []string
			for _, c := range tc.Spec.Components {
				weight := 1.0
				if c.Weight != nil {
					weight = *c.Weight
				}
				spec.Components = append(spec.Components, &evalsiv1alpha1.RewardComponent{
					Ref: c.Ref, Name: c.Name, Weight: weight, Gate: c.Gate, Threshold: c.Threshold,
				})
				key := c.Name
				if key == "" {
					key = catalog.ShortName(c.Ref)
				}
				keys = append(keys, key)
			}
			got := map[string]*evalsiv1alpha1.RewardComponentResult{}
			for k, v := range tc.Components {
				got[k] = &evalsiv1alpha1.RewardComponentResult{Status: status[v.Status], Value: v.Value, Reason: v.Reason}
			}
			reward, err := Compose(spec, keys, got)
			if tc.Want.Error {
				if err == nil {
					t.Fatalf("want an error, got %v", reward)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if reward.GetGated() != tc.Want.Gated {
				t.Errorf("gated = %v, want %v", reward.GetGated(), tc.Want.Gated)
			}
			switch {
			case tc.Want.Total == nil && reward.Total != nil:
				t.Errorf("total = %v, want unset", reward.GetTotal())
			case tc.Want.Total != nil && (reward.Total == nil || math.Abs(reward.GetTotal()-*tc.Want.Total) > 1e-9):
				t.Errorf("total = %v, want %v", reward.Total, *tc.Want.Total)
			}
		})
	}
}

func params(t *testing.T, props ...string) *structpb.Struct {
	t.Helper()
	p := map[string]any{}
	for _, name := range props {
		p[name] = map[string]any{}
	}
	s, err := structpb.NewStruct(map[string]any{"type": "object", "properties": p})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func manifests(t *testing.T) []*evalsiv1alpha1.EvaluatorManifest {
	passed := evalsiv1alpha1.ScoreType_SCORE_TYPE_PASSED
	number := evalsiv1alpha1.ScoreType_SCORE_TYPE_NUMBER
	return []*evalsiv1alpha1.EvaluatorManifest{
		{Name: "builtin/format-check", Version: "1.0.0", Scope: evalsiv1alpha1.Scope_SCOPE_RECORD,
			Outputs: []*evalsiv1alpha1.MetricSpec{{Name: "format-check", Type: passed}}, ParamsSchema: params(t, "pattern")},
		{Name: "builtin/code-exec-tests", Version: "1.0.0", Scope: evalsiv1alpha1.Scope_SCOPE_RECORD,
			Requires: &evalsiv1alpha1.Requirements{Output: true, Isolation: evalsiv1alpha1.IsolationLevel_ISOLATION_LEVEL_CONFINED},
			Outputs:  []*evalsiv1alpha1.MetricSpec{{Name: "code-exec-tests", Type: number}}, ParamsSchema: params(t, "timeout_s", "min_isolation")},
		{Name: "builtin/label", Version: "1.0.0", Scope: evalsiv1alpha1.Scope_SCOPE_RECORD,
			Outputs: []*evalsiv1alpha1.MetricSpec{{Name: "label", Type: evalsiv1alpha1.ScoreType_SCORE_TYPE_LABEL}}, ParamsSchema: params(t)},
		{Name: "test/corpus", Version: "1.0.0", Scope: evalsiv1alpha1.Scope_SCOPE_DATASET,
			Outputs: []*evalsiv1alpha1.MetricSpec{{Name: "corpus", Type: number}}, ParamsSchema: params(t)},
	}
}

// fakeWorker grades format-check by an <answer> tag and code-exec-tests by
// the output's length; "down" in the output is an infrastructure error.
type fakeWorker struct {
	calls   atomic.Int64
	records atomic.Int64
	mu      sync.Mutex
	params  []*structpb.Struct
	block   chan struct{}
}

func (f *fakeWorker) Describe(context.Context) ([]*evalsiv1alpha1.EvaluatorManifest, error) {
	return nil, nil
}

func (f *fakeWorker) Evaluate(ctx context.Context, req *pluginv1alpha1.EvaluateRequest) (*pluginv1alpha1.EvaluateResponse, error) {
	f.calls.Add(1)
	f.records.Add(int64(len(req.GetRecords())))
	f.mu.Lock()
	f.params = append(f.params, req.GetParams())
	f.mu.Unlock()
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	resp := &pluginv1alpha1.EvaluateResponse{BatchId: req.GetBatchId()}
	for _, r := range req.GetRecords() {
		out := r.GetOutput().GetText()
		res := &evalsiv1alpha1.EvaluationResult{RecordId: r.GetId(), Outcome: evalsiv1alpha1.Outcome_OUTCOME_SCORED}
		switch {
		case strings.Contains(out, "down"):
			res.Outcome, res.Reason = evalsiv1alpha1.Outcome_OUTCOME_ERROR, "SandboxError: sandbox unavailable"
		case req.GetEvaluator() == "builtin/format-check":
			res.Scores = []*evalsiv1alpha1.Score{{Name: "format-check", Value: &evalsiv1alpha1.Score_Passed{Passed: strings.HasPrefix(out, "<answer>")}}}
		case req.GetEvaluator() == "builtin/code-exec-tests":
			if r.GetMetadata()["tests"] == nil {
				res.Outcome, res.Reason = evalsiv1alpha1.Outcome_OUTCOME_SKIPPED, "no test cases"
				break
			}
			res.Scores = []*evalsiv1alpha1.Score{{Name: "code-exec-tests", Value: &evalsiv1alpha1.Score_Number{Number: float64(len(out)) / 10}}}
		case req.GetEvaluator() == "builtin/label":
			res.Scores = []*evalsiv1alpha1.Score{{Name: "label", Value: &evalsiv1alpha1.Score_Label{Label: "x"}}}
		}
		resp.Results = append(resp.Results, res)
	}
	return resp, nil
}

func (f *fakeWorker) Reduce(context.Context, *pluginv1alpha1.ReduceRequest) (*pluginv1alpha1.ReduceResponse, error) {
	return nil, errors.New("not used")
}

func (f *fakeWorker) Generate(context.Context, *pluginv1alpha1.GenerateRequest) (*pluginv1alpha1.GenerateResponse, error) {
	return nil, errors.New("not used")
}

func (f *fakeWorker) LoadDataset(context.Context, *pluginv1alpha1.LoadDatasetRequest) ([]*evalsiv1alpha1.Record, error) {
	return nil, errors.New("not used")
}

func (f *fakeWorker) RunTask(context.Context, *pluginv1alpha1.RunTaskRequest, func(*harnessv1alpha1.TrajectoryEvent)) (*pluginv1alpha1.TaskResult, error) {
	return nil, errors.New("not used")
}

func newService(t *testing.T, w *fakeWorker, opts config.Rewards) *Service {
	t.Helper()
	eval := evaluation.New(w, catalog.New(manifests(t)), nil, "", config.Evaluate{BatchSize: 4, Parallelism: 4, MaxRecords: 100})
	if opts.MaxRollouts == 0 {
		opts = config.Rewards{MaxRollouts: 100, MaxInflight: 100, CacheSize: 1000}
	}
	return New(eval, opts, config.Quotas{})
}

func rollout(output string, tests bool) *evalsiv1alpha1.Record {
	r := &evalsiv1alpha1.Record{Output: &evalsiv1alpha1.Content{Kind: &evalsiv1alpha1.Content_Text{Text: output}}}
	if tests {
		r.Metadata = map[string]*structpb.Value{"tests": structpb.NewStringValue("[]")}
	}
	return r
}

func codeSpec() *evalsiv1alpha1.RewardSpec {
	return &evalsiv1alpha1.RewardSpec{Name: "code", Components: []*evalsiv1alpha1.RewardComponent{
		{Ref: "format-check", Weight: 0.1, Gate: true},
		{Ref: "code-exec-tests", Weight: 0.9, MinIsolation: "namespaced"},
	}}
}

func score(t *testing.T, s *Service, spec *evalsiv1alpha1.RewardSpec, rollouts ...*evalsiv1alpha1.Record) ([]*evalsiv1alpha1.Reward, error) {
	t.Helper()
	resp, err := s.ScoreRewards(context.Background(), connect.NewRequest(&evalsiv1alpha1.ScoreRewardsRequest{Spec: spec, Rollouts: rollouts}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetRewards(), nil
}

func TestScoreRewards(t *testing.T) {
	w := &fakeWorker{}
	s := newService(t, w, config.Rewards{})
	got, err := score(t, s, codeSpec(),
		rollout("<answer>12345", true), // format passes; 13 chars -> 1.3
		rollout("12345", true),         // format fails: the floor
		rollout("<answer>", false),     // no tests: skipped, adds nothing
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		total float64
		gated bool
	}{{0.1 + 0.9*1.3, false}, {0, true}, {0.1, false}}
	for i, w := range want {
		if math.Abs(got[i].GetTotal()-w.total) > 1e-9 || got[i].GetGated() != w.gated {
			t.Errorf("rollout %d: total %v gated %v, want %v %v", i, got[i].GetTotal(), got[i].GetGated(), w.total, w.gated)
		}
		if got[i].GetRolloutId() != strconv.Itoa(i) {
			t.Errorf("rollout %d has id %q", i, got[i].GetRolloutId())
		}
	}
	if st := got[2].GetComponents()["code-exec-tests"].GetStatus(); st != evalsiv1alpha1.RewardComponentStatus_REWARD_COMPONENT_STATUS_SKIPPED {
		t.Errorf("skipped component has status %v", st)
	}
	// min_isolation reached the sandboxed evaluator as a param.
	found := false
	for _, p := range w.params {
		if p.GetFields()["min_isolation"].GetStringValue() == "namespaced" {
			found = true
		}
	}
	if !found {
		t.Errorf("min_isolation was not passed to code-exec-tests: %v", w.params)
	}
}

func TestScoreRewardsCachesByContent(t *testing.T) {
	w := &fakeWorker{}
	s := newService(t, w, config.Rewards{})
	spec := codeSpec()
	if _, err := score(t, s, spec, rollout("<answer>1", true), rollout("<answer>1", true)); err != nil {
		t.Fatal(err)
	}
	before := w.records.Load()
	got, err := score(t, s, spec, rollout("<answer>1", true))
	if err != nil {
		t.Fatal(err)
	}
	if w.records.Load() != before {
		t.Errorf("a repeated rollout was scored again")
	}
	if !got[0].GetComponents()["format-check"].GetCached() {
		t.Errorf("cached result not marked: %v", got[0])
	}
	spec.DisableCache = true
	if _, err := score(t, s, spec, rollout("<answer>1", true)); err != nil {
		t.Fatal(err)
	}
	if w.records.Load() == before {
		t.Errorf("disable_cache still used the cache")
	}
	var b strings.Builder
	s.WriteMetrics(&b)
	if !strings.Contains(b.String(), "evalsi_reward_cache_hits_total 2") {
		t.Errorf("metrics:\n%s", b.String())
	}
}

func TestScoreRewardsErrors(t *testing.T) {
	s := newService(t, &fakeWorker{}, config.Rewards{})
	_, err := score(t, s, codeSpec(), rollout("<answer>down", true))
	if connect.CodeOf(err) != connect.CodeUnavailable || !strings.Contains(err.Error(), "sandbox unavailable") {
		t.Fatalf("want Unavailable naming the failure, got %v", err)
	}
	spec := codeSpec()
	spec.OnError = evalsiv1alpha1.RewardOnError_REWARD_ON_ERROR_NONE
	spec.Components[0].Gate = false
	got, err := score(t, s, spec, rollout("<answer>down", true))
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Total != nil {
		t.Errorf("total = %v, want unset", got[0].GetTotal())
	}

	bad := []struct {
		name string
		spec *evalsiv1alpha1.RewardSpec
		want string
	}{
		{"empty", &evalsiv1alpha1.RewardSpec{}, "at least one component"},
		{"unknown", &evalsiv1alpha1.RewardSpec{Components: []*evalsiv1alpha1.RewardComponent{{Ref: "nope"}}}, "nope"},
		{"isolation on a pure evaluator", &evalsiv1alpha1.RewardSpec{Components: []*evalsiv1alpha1.RewardComponent{{Ref: "format-check", MinIsolation: "vm"}}}, "sandbox"},
		{"dataset scope", &evalsiv1alpha1.RewardSpec{Components: []*evalsiv1alpha1.RewardComponent{{Ref: "test/corpus"}}}, "record scope"},
		{"metric", &evalsiv1alpha1.RewardSpec{Components: []*evalsiv1alpha1.RewardComponent{{Ref: "format-check", Metric: "x"}}}, "no metric"},
		{"weight", &evalsiv1alpha1.RewardSpec{Components: []*evalsiv1alpha1.RewardComponent{{Ref: "format-check", Weight: math.Inf(1)}}}, "finite"},
		{"clip", &evalsiv1alpha1.RewardSpec{Clip: &evalsiv1alpha1.RewardClip{Low: 1}, Components: []*evalsiv1alpha1.RewardComponent{{Ref: "format-check"}}}, "clip"},
		{"twice", &evalsiv1alpha1.RewardSpec{Components: []*evalsiv1alpha1.RewardComponent{{Ref: "format-check"}, {Ref: "format-check"}}}, "more than once"},
	}
	for _, tc := range bad {
		_, err := score(t, s, tc.spec, rollout("x", false))
		if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want InvalidArgument with %q", tc.name, err, tc.want)
		}
	}
	got, err = score(t, s, &evalsiv1alpha1.RewardSpec{OnError: evalsiv1alpha1.RewardOnError_REWARD_ON_ERROR_ZERO,
		Components: []*evalsiv1alpha1.RewardComponent{{Ref: "label", Weight: 1}}}, rollout("x", false))
	if err != nil || got[0].GetComponents()["label"].GetStatus() != evalsiv1alpha1.RewardComponentStatus_REWARD_COMPONENT_STATUS_ERROR {
		t.Errorf("a label metric must be an error component: %v %v", got, err)
	}

	small := newService(t, &fakeWorker{}, config.Rewards{MaxRollouts: 1, MaxInflight: 1, CacheSize: 0})
	_, err = score(t, small, codeSpec(), rollout("a", true), rollout("b", true))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("over max_rollouts: %v", err)
	}
}

// TestScoreRewardsBackPressure: calls past max_inflight wait, and give up
// with their context.
func TestScoreRewardsBackPressure(t *testing.T) {
	w := &fakeWorker{block: make(chan struct{})}
	s := newService(t, w, config.Rewards{MaxRollouts: 2, MaxInflight: 2, CacheSize: 0})
	first := make(chan error, 1)
	go func() {
		_, err := score(t, s, codeSpec(), rollout("<answer>a", true), rollout("<answer>b", true))
		first <- err
	}()
	for w.calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := s.ScoreRewards(ctx, connect.NewRequest(&evalsiv1alpha1.ScoreRewardsRequest{Spec: codeSpec(), Rollouts: []*evalsiv1alpha1.Record{rollout("<answer>c", true)}}))
	if connect.CodeOf(err) != connect.CodeDeadlineExceeded {
		t.Fatalf("a call over capacity should wait and time out, got %v", err)
	}
	close(w.block)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if _, err := score(t, s, codeSpec(), rollout("<answer>c", true)); err != nil {
		t.Fatalf("capacity was not released: %v", err)
	}
}

// TestScoreRewardsProjectQuota: a project's max_reward_rollouts bounds its
// calls in flight; other projects are not held back.
func TestScoreRewardsProjectQuota(t *testing.T) {
	w := &fakeWorker{block: make(chan struct{})}
	eval := evaluation.New(w, catalog.New(manifests(t)), nil, "", config.Evaluate{BatchSize: 4, Parallelism: 4, MaxRecords: 100})
	s := New(eval, config.Rewards{MaxRollouts: 100, MaxInflight: 100}, config.Quotas{
		Projects: map[string]config.QuotaLimits{"small": {MaxRewardRollouts: 2}},
	})
	call := func(ctx context.Context, project string, n int) error {
		var rollouts []*evalsiv1alpha1.Record
		for i := 0; i < n; i++ {
			rollouts = append(rollouts, rollout("<answer>x"+strconv.Itoa(i), true))
		}
		_, err := s.ScoreRewards(ctx, connect.NewRequest(&evalsiv1alpha1.ScoreRewardsRequest{Project: project, Spec: codeSpec(), Rollouts: rollouts}))
		return err
	}
	if err := call(context.Background(), "small", 3); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("a batch over the quota: %v", err)
	}
	first := make(chan error, 1)
	go func() { first <- call(context.Background(), "small", 2) }()
	for w.calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := call(ctx, "small", 1); connect.CodeOf(err) != connect.CodeDeadlineExceeded {
		t.Fatalf("the project is at its quota; want a wait, got %v", err)
	}
	other := make(chan error, 1)
	go func() { other <- call(context.Background(), "", 5) }()
	for w.calls.Load() < 3 {
		time.Sleep(time.Millisecond)
	}
	close(w.block)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-other; err != nil {
		t.Fatal(err)
	}
}
