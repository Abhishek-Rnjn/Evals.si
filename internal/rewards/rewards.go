// Package rewards implements RewardService: rewards for RL trainers, built
// from evaluator scores.
//
// A request carries a RewardSpec and a batch of rollouts. Each component is
// an evaluator; components run through the same worker path as Evaluate
// (batched, in parallel, on the cpu or sandbox pools), component scores are
// cached by content, and the total is composed here with the rules of the
// Python library's evalsi.rewards.compose (testdata/reward_vectors.json
// checks both). Calls past max_inflight rollouts wait, which pushes back on
// the trainer instead of overloading the sandboxes.
package rewards

import (
	"container/list"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/config"
	"github.com/abhishek-rnjn/evals.si/internal/evaluation"
)

// Service implements evalsiv1alpha1connect.RewardServiceHandler.
type Service struct {
	eval     *evaluation.Service
	opts     config.Rewards
	quotas   config.Quotas
	inflight *semaphore.Weighted
	cache    *cache

	mu       sync.Mutex
	projects map[string]*semaphore.Weighted

	rollouts, requests, failures atomic.Int64
	waitNanos, scoreNanos        atomic.Int64
}

// New builds a reward service over an evaluation service.
func New(eval *evaluation.Service, opts config.Rewards, quotas config.Quotas) *Service {
	return &Service{
		eval:     eval,
		opts:     opts,
		quotas:   quotas,
		inflight: semaphore.NewWeighted(int64(opts.MaxInflight)),
		cache:    newCache(opts.CacheSize),
		projects: map[string]*semaphore.Weighted{},
	}
}

// projectLimit is the project's in-flight limiter and its size, or nil
// when the project has no max_reward_rollouts quota.
func (s *Service) projectLimit(project string) (*semaphore.Weighted, int) {
	if project == "" {
		project = "default"
	}
	limit := s.quotas.For(project).MaxRewardRollouts
	if limit <= 0 {
		return nil, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sem, ok := s.projects[project]
	if !ok {
		sem = semaphore.NewWeighted(int64(limit))
		s.projects[project] = sem
	}
	return sem, limit
}

func invalid(format string, args ...any) error {
	return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(format, args...))
}

// Refs lists the spec's evaluator references, for access rules.
func Refs(spec *evalsiv1alpha1.RewardSpec) []*evalsiv1alpha1.EvaluatorRef {
	refs := make([]*evalsiv1alpha1.EvaluatorRef, 0, len(spec.GetComponents()))
	for _, c := range spec.GetComponents() {
		refs = append(refs, &evalsiv1alpha1.EvaluatorRef{Ref: c.GetRef(), Name: c.GetName(), Params: c.GetParams()})
	}
	return refs
}

// component is one bound component of a request.
type component struct {
	spec   *evalsiv1alpha1.RewardComponent
	inst   evaluation.Instance
	key    string
	prefix []byte
}

func (s *Service) bind(spec *evalsiv1alpha1.RewardSpec) ([]component, error) {
	if len(spec.GetComponents()) == 0 {
		return nil, invalid("a reward spec needs at least one component")
	}
	if c := spec.GetClip(); c != nil && c.GetLow() > c.GetHigh() {
		return nil, invalid("clip.low must not exceed clip.high")
	}
	refs := Refs(spec)
	for i, c := range spec.GetComponents() {
		if math.IsNaN(c.GetWeight()) || math.IsInf(c.GetWeight(), 0) {
			return nil, invalid("component %q: weight must be a finite number", c.GetRef())
		}
		if c.GetMinIsolation() == "" {
			continue
		}
		params := proto.Clone(refs[i]).(*evalsiv1alpha1.EvaluatorRef).GetParams()
		if params == nil {
			params = &structpb.Struct{}
		}
		if params.Fields == nil {
			params.Fields = map[string]*structpb.Value{}
		}
		if _, set := params.Fields["min_isolation"]; !set {
			params.Fields["min_isolation"] = structpb.NewStringValue(c.GetMinIsolation())
		}
		refs[i].Params = params
	}
	insts, err := s.eval.Bind(refs, spec.GetJudge())
	if err != nil {
		var ce *connect.Error
		if errors.As(err, &ce) && strings.Contains(ce.Message(), "unknown param \"min_isolation\"") {
			return nil, invalid("%s; min_isolation applies only to evaluators that run code in the sandbox", ce.Message())
		}
		return nil, err
	}
	out := make([]component, len(insts))
	for i, in := range insts {
		if in.Dataset() {
			return nil, invalid("component %q is a dataset-scope evaluator; rewards need record scope", in.Name)
		}
		c := spec.GetComponents()[i]
		if m := c.GetMetric(); m != "" && !hasMetric(in.Manifest, m) {
			return nil, invalid("component %q: %s has no metric %q", in.Name, in.Manifest.GetName(), m)
		}
		params, err := proto.MarshalOptions{Deterministic: true}.Marshal(in.Params)
		if err != nil {
			return nil, err
		}
		prefix := fmt.Sprintf("%s@%s\x00%s\x00%s\x00", in.Manifest.GetName(), in.Manifest.GetVersion(), c.GetMetric(), in.Judge)
		out[i] = component{spec: c, inst: in, key: in.Name, prefix: append([]byte(prefix), params...)}
	}
	return out, nil
}

func hasMetric(m *evalsiv1alpha1.EvaluatorManifest, name string) bool {
	for _, o := range m.GetOutputs() {
		if o.GetName() == name {
			return true
		}
	}
	return false
}

// ScoreRewards scores a batch of rollouts.
func (s *Service) ScoreRewards(ctx context.Context, req *connect.Request[evalsiv1alpha1.ScoreRewardsRequest]) (*connect.Response[evalsiv1alpha1.ScoreRewardsResponse], error) {
	msg := req.Msg
	n := len(msg.GetRollouts())
	if n > s.opts.MaxRollouts {
		return nil, invalid("%d rollouts exceed this server's limit of %d per call", n, s.opts.MaxRollouts)
	}
	comps, err := s.bind(msg.GetSpec())
	if err != nil {
		return nil, err
	}
	if err := evaluation.NormalizeIDs(msg.GetRollouts(), 0, map[string]bool{}); err != nil {
		return nil, err
	}
	s.requests.Add(1)
	waited := time.Now()
	weight := int64(max(n, 1))
	if sem, limit := s.projectLimit(msg.GetProject()); sem != nil {
		if n > limit {
			return nil, connect.NewError(connect.CodeResourceExhausted, fmt.Errorf(
				"%d rollouts exceed the project's quota of %d in flight (max_reward_rollouts); send smaller batches", n, limit))
		}
		if err := sem.Acquire(ctx, weight); err != nil {
			return nil, connect.NewError(connect.CodeDeadlineExceeded, fmt.Errorf("waiting for the project's reward quota: %w", err))
		}
		defer sem.Release(weight)
	}
	if err := s.inflight.Acquire(ctx, weight); err != nil {
		return nil, connect.NewError(connect.CodeDeadlineExceeded, fmt.Errorf("waiting for reward capacity: %w", err))
	}
	defer s.inflight.Release(weight)
	s.waitNanos.Add(int64(time.Since(waited)))
	started := time.Now()
	results, err := s.score(ctx, msg.GetSpec(), comps, msg.GetRollouts())
	s.scoreNanos.Add(int64(time.Since(started)))
	if err != nil {
		s.failures.Add(1)
		return nil, err
	}
	s.rollouts.Add(int64(n))
	return connect.NewResponse(&evalsiv1alpha1.ScoreRewardsResponse{Rewards: results}), nil
}

func (s *Service) score(ctx context.Context, spec *evalsiv1alpha1.RewardSpec, comps []component, rollouts []*evalsiv1alpha1.Record) ([]*evalsiv1alpha1.Reward, error) {
	useCache := !spec.GetDisableCache() && s.cache != nil
	results := make([][]*evalsiv1alpha1.RewardComponentResult, len(rollouts))
	for i := range results {
		results[i] = make([]*evalsiv1alpha1.RewardComponentResult, len(comps))
	}
	var bodies [][]byte
	if useCache {
		bodies = make([][]byte, len(rollouts))
		for i, r := range rollouts {
			anon := proto.Clone(r).(*evalsiv1alpha1.Record)
			anon.Id = ""
			b, err := proto.MarshalOptions{Deterministic: true}.Marshal(anon)
			if err != nil {
				return nil, err
			}
			bodies[i] = b
		}
	}
	g, gctx := errgroup.WithContext(ctx)
	for ci, c := range comps {
		var misses []int
		ckeys := make([]string, len(rollouts))
		for ri := range rollouts {
			if useCache {
				ckeys[ri] = cacheKey(c.prefix, bodies[ri])
				if hit, ok := s.cache.get(ckeys[ri]); ok {
					hit = proto.Clone(hit).(*evalsiv1alpha1.RewardComponentResult)
					hit.Cached = true
					results[ri][ci] = hit
					continue
				}
			}
			misses = append(misses, ri)
		}
		if len(misses) == 0 {
			continue
		}
		g.Go(func() error {
			batch := make([]*evalsiv1alpha1.Record, len(misses))
			for k, ri := range misses {
				batch[k] = rollouts[ri]
			}
			got, err := s.eval.RunRecords(gctx, []evaluation.Instance{c.inst}, batch)
			if err != nil {
				return err
			}
			for k, ri := range misses {
				r := componentResult(c.spec, got[k])
				results[ri][ci] = r
				if useCache && r.GetStatus() != evalsiv1alpha1.RewardComponentStatus_REWARD_COMPONENT_STATUS_ERROR {
					s.cache.put(ckeys[ri], r)
				}
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	out := make([]*evalsiv1alpha1.Reward, len(rollouts))
	for ri, r := range rollouts {
		byKey := make(map[string]*evalsiv1alpha1.RewardComponentResult, len(comps))
		for ci, c := range comps {
			byKey[c.key] = results[ri][ci]
		}
		reward, err := Compose(spec, keys(comps), byKey)
		if err != nil {
			return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("rollout %s: %w", r.GetId(), err))
		}
		reward.RolloutId = r.GetId()
		out[ri] = reward
	}
	return out, nil
}

func keys(comps []component) []string {
	out := make([]string, len(comps))
	for i, c := range comps {
		out[i] = c.key
	}
	return out
}

func cacheKey(prefix, body []byte) string {
	h := sha256.New()
	h.Write(prefix)
	h.Write([]byte{0})
	h.Write(body)
	return string(h.Sum(nil))
}

// componentResult reads a component's value from its evaluation result.
func componentResult(c *evalsiv1alpha1.RewardComponent, r *evalsiv1alpha1.EvaluationResult) *evalsiv1alpha1.RewardComponentResult {
	switch r.GetOutcome() {
	case evalsiv1alpha1.Outcome_OUTCOME_SCORED:
	case evalsiv1alpha1.Outcome_OUTCOME_SKIPPED:
		return &evalsiv1alpha1.RewardComponentResult{Status: evalsiv1alpha1.RewardComponentStatus_REWARD_COMPONENT_STATUS_SKIPPED, Reason: r.GetReason()}
	default:
		reason := r.GetReason()
		if reason == "" {
			reason = r.GetOutcome().String()
		}
		return errorResult(reason)
	}
	scores := r.GetScores()
	if len(scores) == 0 {
		return errorResult("evaluator returned no scores")
	}
	score := scores[0]
	if m := c.GetMetric(); m != "" {
		score = nil
		for _, s := range scores {
			if s.GetName() == m {
				score = s
				break
			}
		}
		if score == nil {
			return errorResult(fmt.Sprintf("no metric %q", m))
		}
	}
	var v float64
	switch val := score.GetValue().(type) {
	case *evalsiv1alpha1.Score_Number:
		v = val.Number
	case *evalsiv1alpha1.Score_Passed:
		if val.Passed {
			v = 1
		}
	default:
		return errorResult(fmt.Sprintf("metric %q is not numeric", score.GetName()))
	}
	if math.IsNaN(v) {
		return errorResult(fmt.Sprintf("metric %q is not numeric", score.GetName()))
	}
	return &evalsiv1alpha1.RewardComponentResult{Status: evalsiv1alpha1.RewardComponentStatus_REWARD_COMPONENT_STATUS_SCORED, Value: proto.Float64(v)}
}

func errorResult(reason string) *evalsiv1alpha1.RewardComponentResult {
	return &evalsiv1alpha1.RewardComponentResult{Status: evalsiv1alpha1.RewardComponentStatus_REWARD_COMPONENT_STATUS_ERROR, Reason: reason}
}

// Compose computes one rollout's reward from its component results, in spec
// order (keys[i] is component i's breakdown key). It returns an error for
// errored components under REWARD_ON_ERROR_RAISE (the default).
func Compose(spec *evalsiv1alpha1.RewardSpec, keys []string, got map[string]*evalsiv1alpha1.RewardComponentResult) (*evalsiv1alpha1.Reward, error) {
	scored := evalsiv1alpha1.RewardComponentStatus_REWARD_COMPONENT_STATUS_SCORED
	var errored []string
	for _, k := range keys {
		if got[k].GetStatus() == evalsiv1alpha1.RewardComponentStatus_REWARD_COMPONENT_STATUS_ERROR {
			errored = append(errored, k+": "+got[k].GetReason())
		}
	}
	onError := spec.GetOnError()
	if len(errored) > 0 && (onError == evalsiv1alpha1.RewardOnError_REWARD_ON_ERROR_UNSPECIFIED || onError == evalsiv1alpha1.RewardOnError_REWARD_ON_ERROR_RAISE) {
		return nil, fmt.Errorf("reward components failed: %s", strings.Join(errored, "; "))
	}
	out := &evalsiv1alpha1.Reward{Components: got}
	for i, c := range spec.GetComponents() {
		r := got[keys[i]]
		threshold := 1.0
		if c.Threshold != nil {
			threshold = c.GetThreshold()
		}
		if c.GetGate() && (r.GetStatus() != scored || r.Value == nil || r.GetValue() < threshold) {
			out.Gated = true
			out.Total = proto.Float64(spec.GetFloor())
			return out, nil
		}
	}
	if len(errored) > 0 && onError == evalsiv1alpha1.RewardOnError_REWARD_ON_ERROR_NONE {
		return out, nil
	}
	total := 0.0
	for i, c := range spec.GetComponents() {
		r := got[keys[i]]
		if r.GetStatus() == scored && r.Value != nil {
			total += c.GetWeight() * r.GetValue()
		}
	}
	if clip := spec.GetClip(); clip != nil {
		total = min(max(total, clip.GetLow()), clip.GetHigh())
	}
	out.Total = proto.Float64(total)
	return out, nil
}

// WriteMetrics writes the service's Prometheus metrics.
func (s *Service) WriteMetrics(w io.Writer) {
	metric := func(name, help, typ string, v any) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n%s %v\n", name, help, name, typ, name, v)
	}
	metric("evalsi_reward_requests_total", "ScoreRewards calls.", "counter", s.requests.Load())
	metric("evalsi_reward_failures_total", "ScoreRewards calls that failed while scoring.", "counter", s.failures.Load())
	metric("evalsi_reward_rollouts_total", "Rollouts scored.", "counter", s.rollouts.Load())
	hits, misses := s.cache.stats()
	metric("evalsi_reward_cache_hits_total", "Component scores served from the cache.", "counter", hits)
	metric("evalsi_reward_cache_misses_total", "Component scores computed.", "counter", misses)
	metric("evalsi_reward_wait_seconds_total", "Time calls waited for capacity (back-pressure).", "counter", float64(s.waitNanos.Load())/1e9)
	metric("evalsi_reward_score_seconds_total", "Time spent scoring.", "counter", float64(s.scoreNanos.Load())/1e9)
}

// cache is a size-bounded LRU of component results.
type cache struct {
	mu           sync.Mutex
	size         int
	order        *list.List
	items        map[string]*list.Element
	hits, misses atomic.Int64
}

type entry struct {
	key   string
	value *evalsiv1alpha1.RewardComponentResult
}

func newCache(size int) *cache {
	if size <= 0 {
		return nil
	}
	return &cache{size: size, order: list.New(), items: map[string]*list.Element{}}
}

func (c *cache) get(key string) (*evalsiv1alpha1.RewardComponentResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[key]
	if !ok {
		c.misses.Add(1)
		return nil, false
	}
	c.hits.Add(1)
	c.order.MoveToFront(e)
	return e.Value.(*entry).value, true
}

func (c *cache) put(key string, v *evalsiv1alpha1.RewardComponentResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		e.Value.(*entry).value = v
		c.order.MoveToFront(e)
		return
	}
	c.items[key] = c.order.PushFront(&entry{key: key, value: v})
	for c.order.Len() > c.size {
		last := c.order.Back()
		c.order.Remove(last)
		delete(c.items, last.Value.(*entry).key)
	}
}

func (c *cache) stats() (int64, int64) {
	if c == nil {
		return 0, 0
	}
	return c.hits.Load(), c.misses.Load()
}
