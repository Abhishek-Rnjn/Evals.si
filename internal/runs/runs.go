// Package runs implements RunService: durable, resumable offline runs.
//
// A run is a dataset snapshot plus a spec. Execution goes trial by trial:
// the target (if any) answers every record, then each evaluator grades the
// answers. Every output and result is stored as soon as it exists under a
// deterministic key, so resuming a run skips finished work, and a server
// restart only costs the in-flight batches.
package runs

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"connectrpc.com/connect"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	pluginv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/plugin/v1alpha1"
	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/auth"
	"github.com/abhishek-rnjn/evals.si/internal/authz"
	"github.com/abhishek-rnjn/evals.si/internal/config"
	"github.com/abhishek-rnjn/evals.si/internal/evaluation"
	"github.com/abhishek-rnjn/evals.si/internal/objstore"
	"github.com/abhishek-rnjn/evals.si/internal/pluginhost"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

// Options configures the manager.
type Options struct {
	// Root that dataset paths are resolved against: a directory or
	// s3://bucket/prefix. Empty disallows paths.
	DatasetsDir string
	// The object-storage client, when DatasetsDir is s3://.
	Objects *objstore.Client
	// Runs executing at once; others wait in PENDING.
	MaxConcurrent int
	Evaluate      config.Evaluate
	Agents        config.Agents
	// Online scores of a policy's stored results, for trace datasets.
	TraceScores func(policy string, results []*evalsiv1alpha1.EvaluationResult) map[string]float64
	Logger      *slog.Logger
	// Called with a copy of each run that reaches a final status (for sinks); must not block.
	OnFinished func(*evalsiv1alpha1.Run)
}

// Manager implements evalsiv1alpha1connect.RunServiceHandler.
type Manager struct {
	store  *store.Store
	worker pluginhost.Worker
	engine *evaluation.Service
	opts   Options
	slots  chan struct{}
	log    *slog.Logger

	mu     sync.Mutex
	active map[string]*activeRun
}

type activeRun struct {
	cancel    context.CancelFunc
	cancelled bool
	done      chan struct{}
	subs      map[chan *evalsiv1alpha1.WatchRunResponse]bool
}

// New builds a manager and marks runs left unfinished by a previous process
// as ERROR, so they can be resumed explicitly.
func New(ctx context.Context, st *store.Store, worker pluginhost.Worker, engine *evaluation.Service, opts Options) (*Manager, error) {
	if opts.MaxConcurrent < 1 {
		opts.MaxConcurrent = 4
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	m := &Manager{
		store: st, worker: worker, engine: engine, opts: opts,
		slots:  make(chan struct{}, opts.MaxConcurrent),
		log:    opts.Logger,
		active: map[string]*activeRun{},
	}
	stale, err := st.RunsWithStatus(ctx, evalsiv1alpha1.RunStatus_RUN_STATUS_PENDING, evalsiv1alpha1.RunStatus_RUN_STATUS_RUNNING)
	if err != nil {
		return nil, err
	}
	for _, run := range stale {
		run.Status = evalsiv1alpha1.RunStatus_RUN_STATUS_ERROR
		run.Error = "the server stopped while this run was in progress; resume it to continue"
		if err := st.UpdateRun(ctx, run); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// Wait blocks until every executing run has stopped (used at shutdown).
func (m *Manager) Wait() {
	m.mu.Lock()
	var done []chan struct{}
	for _, a := range m.active {
		done = append(done, a.done)
	}
	m.mu.Unlock()
	for _, d := range done {
		<-d
	}
}

// Shutdown cancels every executing run (it becomes ERROR, resumable) and waits.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	for _, a := range m.active {
		a.cancel()
	}
	m.mu.Unlock()
	m.Wait()
}

func invalid(format string, args ...any) error {
	return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(format, args...))
}

func terminal(s evalsiv1alpha1.RunStatus) bool {
	switch s {
	case evalsiv1alpha1.RunStatus_RUN_STATUS_SUCCEEDED, evalsiv1alpha1.RunStatus_RUN_STATUS_FAILED,
		evalsiv1alpha1.RunStatus_RUN_STATUS_ERROR, evalsiv1alpha1.RunStatus_RUN_STATUS_CANCELLED:
		return true
	}
	return false
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "run-" + hex.EncodeToString(b)
}

// validate checks a spec before anything is stored.
func (m *Manager) validate(spec *evalsiv1alpha1.RunSpec) ([]evaluation.Instance, error) {
	if spec == nil {
		return nil, invalid("spec is required")
	}
	insts, err := m.engine.Bind(spec.GetEvaluators(), spec.GetJudge())
	if err != nil {
		return nil, err
	}
	if spec.GetDataset().GetSource() == nil {
		return nil, invalid("spec.dataset needs one of inline, path or uri")
	}
	if spec.GetTrials() < 0 {
		return nil, invalid("spec.trials cannot be negative")
	}
	if authz.AgentRun(spec) {
		if err := m.validateAgent(spec); err != nil {
			return nil, err
		}
	} else if t := spec.GetTarget(); t != nil {
		switch {
		case t.GetConnector() != "openai-compatible" && t.GetConnector() != "anthropic":
			return nil, invalid("spec.target.connector must be openai-compatible or anthropic")
		case t.GetModel() == "":
			return nil, invalid("spec.target.model is required")
		case t.GetConnector() == "openai-compatible" && t.GetBaseUrl() == "":
			return nil, invalid("spec.target.base_url is required for openai-compatible")
		}
	} else if spec.GetTrials() > 1 {
		return nil, invalid("spec.trials above 1 needs a target; without one every trial is identical")
	}
	for _, g := range spec.GetGates() {
		if g.GetMetric() == "" || (g.Min == nil && g.Max == nil) {
			return nil, invalid("every gate needs a metric and a min or max")
		}
	}
	return insts, nil
}

// resolvePath keeps dataset paths inside DatasetsDir, symlinks included. On
// object storage the result is an s3:// URL, which the worker side fetches.
func (m *Manager) resolvePath(rel string) (string, error) {
	if m.opts.DatasetsDir == "" {
		return "", invalid("this server does not accept dataset paths; set datasets_dir in its config, or send records inline")
	}
	if loc, ok := objstore.Parse(m.opts.DatasetsDir); ok {
		clean := path.Clean(filepath.ToSlash(rel))
		if path.IsAbs(clean) || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
			return "", invalid("dataset %q must be a path inside the server's datasets_dir", rel)
		}
		return loc.Join(clean).String(), nil
	}
	root, err := filepath.EvalSymlinks(m.opts.DatasetsDir)
	if err != nil {
		return "", fmt.Errorf("datasets_dir: %w", err)
	}
	if filepath.IsAbs(rel) {
		return "", invalid("dataset paths must be relative to the server's datasets_dir")
	}
	full, err := filepath.EvalSymlinks(filepath.Join(root, rel))
	if err != nil {
		return "", invalid("dataset %q: %v", rel, errors.Unwrap(err))
	}
	if r, err := filepath.Rel(root, full); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(os.PathSeparator)) {
		return "", invalid("dataset %q is outside the server's datasets_dir", rel)
	}
	return full, nil
}

// resolveURI passes hf:// through and confines importer URIs
// (scheme://path?options, for example inspect://logs/run.eval), which name
// local files, to DatasetsDir like plain paths.
func (m *Manager) resolveURI(uri string) (string, error) {
	scheme, rest, ok := strings.Cut(uri, "://")
	if !ok || scheme == "" {
		return "", invalid("dataset uri %q needs a scheme such as hf:// or inspect://; use path for files", uri)
	}
	if scheme == "hf" {
		return uri, nil
	}
	path, query, hasQuery := strings.Cut(rest, "?")
	full, err := m.resolvePath(path)
	if err != nil {
		return "", err
	}
	out := scheme + "://" + full
	if hasQuery {
		out += "?" + query
	}
	return out, nil
}

func (m *Manager) loadDataset(ctx context.Context, src *evalsiv1alpha1.DatasetSource, project string) ([]*evalsiv1alpha1.Record, error) {
	var records []*evalsiv1alpha1.Record
	var err error
	switch s := src.GetSource().(type) {
	case *evalsiv1alpha1.DatasetSource_Inline:
		records = s.Inline.GetRecords()
		if n := int(src.GetLimit()); n > 0 && n < len(records) {
			records = records[:n]
		}
	case *evalsiv1alpha1.DatasetSource_Traces:
		if records, err = m.traceRecords(ctx, s.Traces, project, int(src.GetLimit())); err != nil {
			return nil, err
		}
	case *evalsiv1alpha1.DatasetSource_Run:
		if records, err = m.runRecords(ctx, s.Run); err != nil {
			return nil, err
		}
		if n := int(src.GetLimit()); n > 0 && n < len(records) {
			records = records[:n]
		}
	default:
		req := proto.Clone(src).(*evalsiv1alpha1.DatasetSource)
		switch s := s.(type) {
		case *evalsiv1alpha1.DatasetSource_Path:
			full, err := m.resolvePath(s.Path)
			if err != nil {
				return nil, err
			}
			req.Source = &evalsiv1alpha1.DatasetSource_Path{Path: full}
		case *evalsiv1alpha1.DatasetSource_Uri:
			uri, err := m.resolveURI(s.Uri)
			if err != nil {
				return nil, err
			}
			req.Source = &evalsiv1alpha1.DatasetSource_Uri{Uri: uri}
		}
		records, err = m.worker.LoadDataset(ctx, &pluginv1alpha1.LoadDatasetRequest{Source: req})
		if err != nil {
			code := connect.CodeOf(err)
			if code != connect.CodeInvalidArgument {
				code = connect.CodeUnavailable
			}
			return nil, connect.NewError(code, fmt.Errorf("loading dataset: %w", err))
		}
	}
	if len(records) == 0 {
		return nil, invalid("the dataset has no records")
	}
	if err := evaluation.NormalizeIDs(records, 0, map[string]bool{}); err != nil {
		return nil, err
	}
	return records, nil
}

func datasetHash(records []*evalsiv1alpha1.Record) string {
	h := sha256.New()
	opts := proto.MarshalOptions{Deterministic: true}
	for _, r := range records {
		b, _ := opts.Marshal(r)
		h.Write(b)
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func trialsOf(spec *evalsiv1alpha1.RunSpec) int {
	return max(int(spec.GetTrials()), 1)
}

func totalTasks(spec *evalsiv1alpha1.RunSpec, insts []evaluation.Instance, records int) int64 {
	perTrial := 0
	for _, in := range insts {
		if in.Dataset() {
			perTrial++
		} else {
			perTrial += records
		}
	}
	if generates(spec) {
		perTrial += records
	}
	return int64(perTrial * trialsOf(spec))
}

// generates reports whether the run produces outputs: a target answers the
// records, or an agent works through them as tasks.
func generates(spec *evalsiv1alpha1.RunSpec) bool {
	return spec.GetTarget() != nil || authz.AgentRun(spec)
}

// DefaultProject holds runs that name no project.
const DefaultProject = "default"

// CreateRun validates, snapshots the dataset, stores the run and starts it.
func (m *Manager) CreateRun(ctx context.Context, req *connect.Request[evalsiv1alpha1.CreateRunRequest]) (*connect.Response[evalsiv1alpha1.CreateRunResponse], error) {
	spec := req.Msg.GetSpec()
	insts, err := m.validate(spec)
	if err != nil {
		return nil, err
	}
	project := projectOr(req.Msg.GetProject())
	records, err := m.loadDataset(ctx, spec.GetDataset(), project)
	if err != nil {
		return nil, err
	}
	run, err := m.createRun(ctx, req.Msg.GetName(), project, req.Msg.GetLabels(), spec, insts, records)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&evalsiv1alpha1.CreateRunResponse{Run: run}), nil
}

// createRun stores a validated run over a dataset snapshot and starts it.
func (m *Manager) createRun(ctx context.Context, name, project string, labels map[string]string, spec *evalsiv1alpha1.RunSpec, insts []evaluation.Instance, records []*evalsiv1alpha1.Record) (*evalsiv1alpha1.Run, error) {
	stored := proto.Clone(spec).(*evalsiv1alpha1.RunSpec)
	if stored.GetDataset().GetInline() != nil {
		// The records live in the snapshot; keep the stored spec small.
		stored.Dataset.Source = &evalsiv1alpha1.DatasetSource_Inline{Inline: &evalsiv1alpha1.InlineRecords{}}
	}
	run := &evalsiv1alpha1.Run{
		Id:            newID(),
		Name:          name,
		Project:       project,
		Labels:        labels,
		CreatedBy:     auth.PrincipalFrom(ctx).ID(),
		Spec:          stored,
		Status:        evalsiv1alpha1.RunStatus_RUN_STATUS_PENDING,
		CreatedAt:     timestamppb.Now(),
		Progress:      &evalsiv1alpha1.Progress{Total: totalTasks(spec, insts, len(records))},
		DatasetSha256: datasetHash(records),
		Records:       int64(len(records)),
	}
	if err := m.store.CreateRun(ctx, run, records); err != nil {
		return nil, err
	}
	m.start(run)
	return run, nil
}

func (m *Manager) start(run *evalsiv1alpha1.Run) {
	ctx, cancel := context.WithCancel(context.Background())
	a := &activeRun{cancel: cancel, done: make(chan struct{}), subs: map[chan *evalsiv1alpha1.WatchRunResponse]bool{}}
	m.mu.Lock()
	m.active[run.GetId()] = a
	m.mu.Unlock()
	go func() {
		defer close(a.done)
		defer cancel()
		select {
		case m.slots <- struct{}{}:
			defer func() { <-m.slots }()
		case <-ctx.Done():
		}
		m.execute(ctx, run.GetId(), a)
		m.mu.Lock()
		delete(m.active, run.GetId())
		for ch := range a.subs {
			close(ch)
		}
		a.subs = nil
		m.mu.Unlock()
	}()
}

// publishRun publishes a snapshot: the run keeps changing after this call.
func (m *Manager) publishRun(a *activeRun, run *evalsiv1alpha1.Run) {
	snapshot := proto.Clone(run).(*evalsiv1alpha1.Run)
	m.publish(a, &evalsiv1alpha1.WatchRunResponse{Event: &evalsiv1alpha1.WatchRunResponse_Run{Run: snapshot}})
}

// publish sends an event to every watcher without blocking. A watcher that
// falls too far behind is dropped; it can reconnect and read the run state.
func (m *Manager) publish(a *activeRun, ev *evalsiv1alpha1.WatchRunResponse) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for ch := range a.subs {
		select {
		case ch <- ev:
		default:
			delete(a.subs, ch)
			close(ch)
		}
	}
}

type execution struct {
	m       *Manager
	a       *activeRun
	run     *evalsiv1alpha1.Run
	spec    *evalsiv1alpha1.RunSpec
	insts   []evaluation.Instance
	records []*evalsiv1alpha1.Record
	outputs map[[2]int]store.Output
	keys    map[store.Key]bool
}

func (m *Manager) execute(ctx context.Context, id string, a *activeRun) {
	run, err := m.store.GetRun(context.Background(), id)
	if err != nil {
		m.log.Error("run vanished", "run", id, "err", err)
		return
	}
	finish := func(status evalsiv1alpha1.RunStatus, msg string) {
		run.Status, run.Error, run.FinishedAt = status, msg, timestamppb.Now()
		if err := m.store.UpdateRun(context.Background(), run); err != nil {
			m.log.Error("saving run", "run", id, "err", err)
		}
		m.publishRun(a, run)
		if m.opts.OnFinished != nil {
			m.opts.OnFinished(proto.Clone(run).(*evalsiv1alpha1.Run))
		}
	}
	if ctx.Err() != nil {
		finish(m.stoppedStatus(a), m.stoppedMessage(a))
		return
	}
	run.Status, run.Error = evalsiv1alpha1.RunStatus_RUN_STATUS_RUNNING, ""
	if run.StartedAt == nil {
		run.StartedAt = timestamppb.Now()
	}
	run.FinishedAt = nil
	if err := m.store.UpdateRun(ctx, run); err != nil {
		finish(evalsiv1alpha1.RunStatus_RUN_STATUS_ERROR, err.Error())
		return
	}
	m.publishRun(a, run)

	ex, err := m.prepare(ctx, run, a)
	if err == nil {
		err = ex.runTrials(ctx)
	}
	if err == nil {
		err = ex.finalize(ctx)
	}
	switch {
	case err == nil:
		status := evalsiv1alpha1.RunStatus_RUN_STATUS_SUCCEEDED
		for _, g := range run.GetGates() {
			if !g.GetPassed() {
				status = evalsiv1alpha1.RunStatus_RUN_STATUS_FAILED
			}
		}
		finish(status, "")
	case ctx.Err() != nil:
		finish(m.stoppedStatus(a), m.stoppedMessage(a))
	default:
		m.log.Warn("run failed", "run", id, "err", err)
		finish(evalsiv1alpha1.RunStatus_RUN_STATUS_ERROR, err.Error())
	}
}

func (m *Manager) stoppedStatus(a *activeRun) evalsiv1alpha1.RunStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a.cancelled {
		return evalsiv1alpha1.RunStatus_RUN_STATUS_CANCELLED
	}
	return evalsiv1alpha1.RunStatus_RUN_STATUS_ERROR
}

func (m *Manager) stoppedMessage(a *activeRun) string {
	if m.stoppedStatus(a) == evalsiv1alpha1.RunStatus_RUN_STATUS_CANCELLED {
		return ""
	}
	return "the server stopped while this run was in progress; resume it to continue"
}

func (m *Manager) prepare(ctx context.Context, run *evalsiv1alpha1.Run, a *activeRun) (*execution, error) {
	insts, err := m.engine.Bind(run.GetSpec().GetEvaluators(), run.GetSpec().GetJudge())
	if err != nil {
		return nil, err
	}
	records, err := m.store.Records(ctx, run.GetId())
	if err != nil {
		return nil, err
	}
	outputs, err := m.store.Outputs(ctx, run.GetId())
	if err != nil {
		return nil, err
	}
	keys, err := m.store.ResultKeys(ctx, run.GetId())
	if err != nil {
		return nil, err
	}
	ex := &execution{m: m, a: a, run: run, spec: run.GetSpec(), insts: insts, records: records, outputs: outputs, keys: keys}
	// Recount progress and spend from what is already stored (resume).
	done := int64(len(keys))
	if generates(run.GetSpec()) {
		done += int64(len(outputs))
	}
	run.Progress = &evalsiv1alpha1.Progress{Total: totalTasks(run.GetSpec(), insts, len(records)), Done: done}
	run.TargetUsage, run.JudgeUsage = &evalsiv1alpha1.Usage{}, &evalsiv1alpha1.Usage{}
	for _, o := range outputs {
		addUsage(run.TargetUsage, o.Record.GetUsage())
	}
	stored, err := m.store.Results(ctx, run.GetId(), "", 0, 0)
	if err != nil {
		return nil, err
	}
	for _, r := range stored {
		ex.addJudgeUsage(r.Result)
	}
	return ex, nil
}

func addUsage(total, part *evalsiv1alpha1.Usage) {
	if part == nil {
		return
	}
	if part.InputTokens != nil {
		total.InputTokens = proto.Int64(total.GetInputTokens() + part.GetInputTokens())
	}
	if part.OutputTokens != nil {
		total.OutputTokens = proto.Int64(total.GetOutputTokens() + part.GetOutputTokens())
	}
}

func tokens(u *evalsiv1alpha1.Usage) int64 { return u.GetInputTokens() + u.GetOutputTokens() }

func (ex *execution) addJudgeUsage(r *evalsiv1alpha1.EvaluationResult) {
	for _, s := range r.GetScores() {
		addUsage(ex.run.JudgeUsage, s.GetCost())
	}
}

func (ex *execution) progress(ctx context.Context, n int) error {
	ex.run.Progress.Done += int64(n)
	ex.m.publish(ex.a, &evalsiv1alpha1.WatchRunResponse{Event: &evalsiv1alpha1.WatchRunResponse_Progress{Progress: proto.Clone(ex.run.Progress).(*evalsiv1alpha1.Progress)}})
	return ex.m.store.UpdateRun(ctx, ex.run)
}

func (ex *execution) checkBudget() error {
	b := ex.spec.GetBudget()
	if b.GetMaxTargetTokens() > 0 && tokens(ex.run.GetTargetUsage()) > b.GetMaxTargetTokens() {
		return fmt.Errorf("budget exceeded: the target used %d tokens, over max_target_tokens %d", tokens(ex.run.GetTargetUsage()), b.GetMaxTargetTokens())
	}
	if b.GetMaxJudgeTokens() > 0 && tokens(ex.run.GetJudgeUsage()) > b.GetMaxJudgeTokens() {
		return fmt.Errorf("budget exceeded: judges used %d tokens, over max_judge_tokens %d", tokens(ex.run.GetJudgeUsage()), b.GetMaxJudgeTokens())
	}
	return nil
}

func (ex *execution) runTrials(ctx context.Context) error {
	for trial := range trialsOf(ex.spec) {
		if err := ex.generate(ctx, trial); err != nil {
			return err
		}
		if err := ex.evaluate(ctx, trial); err != nil {
			return err
		}
	}
	return nil
}

// generate asks the target to answer every record that has no output for this trial yet.
func (ex *execution) generate(ctx context.Context, trial int) error {
	if authz.AgentRun(ex.spec) {
		return ex.runTasks(ctx, trial)
	}
	target := ex.spec.GetTarget()
	if target == nil {
		return nil
	}
	var missing []int
	for i := range ex.records {
		if _, ok := ex.outputs[[2]int{i, trial}]; !ok {
			missing = append(missing, i)
		}
	}
	opts := ex.m.opts.Evaluate
	window := opts.BatchSize * opts.Parallelism
	for start := 0; start < len(missing); start += window {
		chunk := missing[start:min(start+window, len(missing))]
		results := make([]store.Output, len(chunk))
		g, gctx := errgroup.WithContext(ctx)
		g.SetLimit(opts.Parallelism)
		for b := 0; b < len(chunk); b += opts.BatchSize {
			batch := chunk[b:min(b+opts.BatchSize, len(chunk))]
			g.Go(func() error {
				req := &pluginv1alpha1.GenerateRequest{Target: target}
				for _, i := range batch {
					req.Records = append(req.Records, ex.records[i])
				}
				resp, err := ex.m.worker.Generate(gctx, req)
				if err != nil {
					return fmt.Errorf("target: %w", err)
				}
				if len(resp.GetResults()) != len(batch) {
					return fmt.Errorf("target: worker returned %d results for %d records", len(resp.GetResults()), len(batch))
				}
				for k, gen := range resp.GetResults() {
					i := batch[k]
					out := store.Output{RecordIdx: i, Trial: trial, Error: gen.GetError()}
					if gen.GetOutput() != nil {
						rec := proto.Clone(ex.records[i]).(*evalsiv1alpha1.Record)
						rec.Output, rec.Usage = gen.GetOutput(), gen.GetUsage()
						rec.Provenance = &evalsiv1alpha1.Provenance{Source: &evalsiv1alpha1.Provenance_Run{
							Run: &evalsiv1alpha1.RunProvenance{RunId: ex.run.GetId(), Trial: int32(trial)},
						}}
						out.Record = rec
					}
					results[b+k] = out
				}
				return nil
			})
		}
		if err := g.Wait(); err != nil {
			return err
		}
		if err := ex.m.store.PutOutputs(ctx, ex.run.GetId(), results); err != nil {
			return err
		}
		for _, o := range results {
			ex.outputs[[2]int{o.RecordIdx, o.Trial}] = o
			addUsage(ex.run.TargetUsage, o.Record.GetUsage())
		}
		if err := ex.progress(ctx, len(results)); err != nil {
			return err
		}
		if err := ex.checkBudget(); err != nil {
			return err
		}
	}
	return nil
}

// evaluate grades this trial's answers, skipping results that already exist.
func (ex *execution) evaluate(ctx context.Context, trial int) error {
	// The records to grade, and errors for records whose generation failed.
	var graded []int
	var failed []store.Result
	recordFor := map[int]*evalsiv1alpha1.Record{}
	for i, rec := range ex.records {
		if !generates(ex.spec) {
			recordFor[i] = rec
			graded = append(graded, i)
			continue
		}
		out := ex.outputs[[2]int{i, trial}]
		if out.Record != nil {
			recordFor[i] = out.Record
			graded = append(graded, i)
			continue
		}
		for ii, in := range ex.insts {
			if !in.Dataset() && !ex.keys[store.Key{RecordIdx: i, Trial: trial, EvalIdx: ii}] {
				failed = append(failed, store.Result{RecordIdx: i, Trial: trial, EvalIdx: ii, Result: &evalsiv1alpha1.EvaluationResult{
					RecordId: rec.GetId(), Evaluator: in.Name,
					EvaluatorRef: in.Manifest.GetName() + "@" + in.Manifest.GetVersion(),
					Outcome:      evalsiv1alpha1.Outcome_OUTCOME_ERROR, Reason: "target failed: " + out.Error, Trial: int32(trial),
				}})
			}
		}
	}
	if err := ex.save(ctx, failed); err != nil {
		return err
	}
	window := ex.m.opts.Evaluate.BatchSize * ex.m.opts.Evaluate.Parallelism
	for ii, in := range ex.insts {
		if in.Dataset() {
			continue
		}
		var todo []int
		for _, i := range graded {
			if !ex.keys[store.Key{RecordIdx: i, Trial: trial, EvalIdx: ii}] {
				todo = append(todo, i)
			}
		}
		for start := 0; start < len(todo); start += window {
			chunk := todo[start:min(start+window, len(todo))]
			recs := make([]*evalsiv1alpha1.Record, len(chunk))
			for k, i := range chunk {
				recs[k] = recordFor[i]
			}
			results, err := ex.m.engine.RunRecords(ctx, []evaluation.Instance{in}, recs)
			if err != nil {
				return err
			}
			batch := make([]store.Result, len(results))
			for k, r := range results {
				r.Trial = int32(trial)
				batch[k] = store.Result{RecordIdx: chunk[k], Trial: trial, EvalIdx: ii, Result: r}
			}
			if err := ex.save(ctx, batch); err != nil {
				return err
			}
		}
	}
	for ii, in := range ex.insts {
		if !in.Dataset() || ex.keys[store.Key{RecordIdx: store.DatasetIndex, Trial: trial, EvalIdx: ii}] {
			continue
		}
		recs := make([]*evalsiv1alpha1.Record, 0, len(graded))
		for _, i := range graded {
			recs = append(recs, recordFor[i])
		}
		results, err := ex.m.engine.RunDataset(ctx, []evaluation.Instance{in}, recs)
		if err != nil {
			return err
		}
		results[0].Trial = int32(trial)
		if err := ex.save(ctx, []store.Result{{RecordIdx: store.DatasetIndex, Trial: trial, EvalIdx: ii, Result: results[0]}}); err != nil {
			return err
		}
	}
	return nil
}

func (ex *execution) save(ctx context.Context, batch []store.Result) error {
	if len(batch) == 0 {
		return nil
	}
	if err := ex.m.store.PutResults(ctx, ex.run.GetId(), batch); err != nil {
		return err
	}
	for _, r := range batch {
		ex.keys[store.Key{RecordIdx: r.RecordIdx, Trial: r.Trial, EvalIdx: r.EvalIdx}] = true
		ex.addJudgeUsage(r.Result)
		ex.m.publish(ex.a, &evalsiv1alpha1.WatchRunResponse{Event: &evalsiv1alpha1.WatchRunResponse_Result{Result: r.Result}})
	}
	if err := ex.progress(ctx, len(batch)); err != nil {
		return err
	}
	return ex.checkBudget()
}

func (ex *execution) finalize(ctx context.Context) error {
	stored, err := ex.m.store.Results(ctx, ex.run.GetId(), "", 0, 0)
	if err != nil {
		return err
	}
	results := make([]*evalsiv1alpha1.EvaluationResult, len(stored))
	for i, r := range stored {
		results[i] = r.Result
	}
	summaries, err := evaluation.Summarize(ex.insts, results, ex.records, ex.spec.GetSummary(), trialsOf(ex.spec))
	if err != nil {
		return err
	}
	ex.run.Summaries = summaries
	ex.run.Gates = CheckGates(ex.spec.GetGates(), summaries)
	return nil
}

// CheckGates mirrors check_gates in the Python SDK.
func CheckGates(gates []*evalsiv1alpha1.Gate, summaries []*evalsiv1alpha1.MetricSummary) []*evalsiv1alpha1.GateResult {
	byMetric := map[string]*evalsiv1alpha1.MetricSummary{}
	for _, s := range summaries {
		byMetric[s.GetMetric()] = s
	}
	out := make([]*evalsiv1alpha1.GateResult, 0, len(gates))
	for _, g := range gates {
		res := &evalsiv1alpha1.GateResult{Gate: g}
		out = append(out, res)
		s, ok := byMetric[g.GetMetric()]
		if !ok {
			res.Reason = "no such metric in this run"
			continue
		}
		stat := "mean"
		var value *float64
		switch g.GetStat() {
		case evalsiv1alpha1.GateStat_GATE_STAT_CI_LOW:
			stat = "ci_low"
			if s.GetCi() != nil {
				value = proto.Float64(s.GetCi().GetLow())
			}
		case evalsiv1alpha1.GateStat_GATE_STAT_CI_HIGH:
			stat = "ci_high"
			if s.GetCi() != nil {
				value = proto.Float64(s.GetCi().GetHigh())
			}
		default:
			value = s.Mean
		}
		res.Value = value
		switch {
		case value == nil:
			res.Reason = "metric has no " + stat
		case g.Min != nil && *value < g.GetMin():
			res.Reason = fmt.Sprintf("%s %.4g is below %.4g", stat, *value, g.GetMin())
		case g.Max != nil && *value > g.GetMax():
			res.Reason = fmt.Sprintf("%s %.4g is above %.4g", stat, *value, g.GetMax())
		default:
			res.Passed = true
		}
	}
	return out
}

func (m *Manager) get(ctx context.Context, id string) (*evalsiv1alpha1.Run, error) {
	run, err := m.store.GetRun(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("no run %q", id))
	}
	return run, err
}

// GetRun returns a run.
func (m *Manager) GetRun(ctx context.Context, req *connect.Request[evalsiv1alpha1.GetRunRequest]) (*connect.Response[evalsiv1alpha1.GetRunResponse], error) {
	run, err := m.get(ctx, req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&evalsiv1alpha1.GetRunResponse{Run: run}), nil
}

// ListRuns lists runs newest first, without specs.
func (m *Manager) ListRuns(ctx context.Context, req *connect.Request[evalsiv1alpha1.ListRunsRequest]) (*connect.Response[evalsiv1alpha1.ListRunsResponse], error) {
	size := int(req.Msg.GetPageSize())
	if size <= 0 || size > 200 {
		size = 50
	}
	projects := authz.Projects(ctx, "runs.read")
	if p := req.Msg.GetProject(); p != "" {
		if projects != nil && !slices.Contains(projects, p) {
			projects = []string{}
		} else {
			projects = []string{p}
		}
	}
	runs, next, err := m.store.ListRuns(ctx, projects, size, req.Msg.GetPageToken())
	if err != nil {
		return nil, invalid("%v", err)
	}
	// Pages can come back short: runs the caller cannot read are dropped.
	runsCode := authz.RunsCodeFrom(ctx)
	visible := runs[:0]
	for _, r := range runs {
		if authz.Can(ctx, "runs.read", r.GetProject(), authz.RunResource(r, runsCode)) {
			r.Spec = nil
			visible = append(visible, r)
		}
	}
	return connect.NewResponse(&evalsiv1alpha1.ListRunsResponse{Runs: visible, NextPageToken: next}), nil
}

// WatchRun streams the run's state, then events until it finishes.
func (m *Manager) WatchRun(ctx context.Context, req *connect.Request[evalsiv1alpha1.WatchRunRequest], stream *connect.ServerStream[evalsiv1alpha1.WatchRunResponse]) error {
	id := req.Msg.GetId()
	// Subscribe before reading the state so no transition is missed.
	ch := make(chan *evalsiv1alpha1.WatchRunResponse, 1024)
	m.mu.Lock()
	a := m.active[id]
	if a != nil {
		a.subs[ch] = true
	}
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		if a != nil && a.subs != nil && a.subs[ch] {
			delete(a.subs, ch)
		}
		m.mu.Unlock()
	}()
	run, err := m.get(ctx, id)
	if err != nil {
		return err
	}
	if err := stream.Send(&evalsiv1alpha1.WatchRunResponse{Event: &evalsiv1alpha1.WatchRunResponse_Run{Run: run}}); err != nil {
		return err
	}
	if a == nil {
		return nil // not executing: the state above is final
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-ch:
			if !ok {
				// The run stopped (or this watcher fell behind): send the final state.
				final, err := m.get(context.Background(), id)
				if err != nil {
					return err
				}
				return stream.Send(&evalsiv1alpha1.WatchRunResponse{Event: &evalsiv1alpha1.WatchRunResponse_Run{Run: final}})
			}
			if ev.GetResult() != nil && !req.Msg.GetIncludeResults() {
				continue
			}
			if err := stream.Send(ev); err != nil {
				return err
			}
			if r := ev.GetRun(); r != nil && terminal(r.GetStatus()) {
				return nil
			}
		}
	}
}

// CancelRun stops an executing run; it can be resumed later.
func (m *Manager) CancelRun(ctx context.Context, req *connect.Request[evalsiv1alpha1.CancelRunRequest]) (*connect.Response[evalsiv1alpha1.CancelRunResponse], error) {
	id := req.Msg.GetId()
	m.mu.Lock()
	a := m.active[id]
	if a != nil {
		a.cancelled = true
		a.cancel()
	}
	m.mu.Unlock()
	if a != nil {
		<-a.done
	}
	run, err := m.get(ctx, id)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&evalsiv1alpha1.CancelRunResponse{Run: run}), nil
}

// ResumeRun continues an errored or cancelled run.
func (m *Manager) ResumeRun(ctx context.Context, req *connect.Request[evalsiv1alpha1.ResumeRunRequest]) (*connect.Response[evalsiv1alpha1.ResumeRunResponse], error) {
	run, err := m.get(ctx, req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	_, running := m.active[run.GetId()]
	m.mu.Unlock()
	if running || (run.GetStatus() != evalsiv1alpha1.RunStatus_RUN_STATUS_ERROR && run.GetStatus() != evalsiv1alpha1.RunStatus_RUN_STATUS_CANCELLED) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("run %s is %s; only errored or cancelled runs can be resumed", run.GetId(), run.GetStatus()))
	}
	run.Status = evalsiv1alpha1.RunStatus_RUN_STATUS_PENDING
	if err := m.store.UpdateRun(ctx, run); err != nil {
		return nil, err
	}
	m.start(run)
	return connect.NewResponse(&evalsiv1alpha1.ResumeRunResponse{Run: run}), nil
}

// ListRunResults pages through a run's results with the graded records.
func (m *Manager) ListRunResults(ctx context.Context, req *connect.Request[evalsiv1alpha1.ListRunResultsRequest]) (*connect.Response[evalsiv1alpha1.ListRunResultsResponse], error) {
	id := req.Msg.GetRunId()
	if _, err := m.get(ctx, id); err != nil {
		return nil, err
	}
	size := int(req.Msg.GetPageSize())
	if size <= 0 || size > 1000 {
		size = 200
	}
	offset := 0
	if t := req.Msg.GetPageToken(); t != "" {
		var err error
		if offset, err = strconv.Atoi(t); err != nil || offset < 0 {
			return nil, invalid("bad page_token")
		}
	}
	page, err := m.store.Results(ctx, id, req.Msg.GetEvaluator(), size+1, offset)
	if err != nil {
		return nil, err
	}
	next := ""
	if len(page) > size {
		page, next = page[:size], strconv.Itoa(offset+size)
	}
	records, err := m.store.Records(ctx, id)
	if err != nil {
		return nil, err
	}
	outputs, err := m.store.Outputs(ctx, id)
	if err != nil {
		return nil, err
	}
	resp := &evalsiv1alpha1.ListRunResultsResponse{NextPageToken: next}
	seen := map[[2]int]bool{}
	for _, r := range page {
		resp.Results = append(resp.Results, r.Result)
		key := [2]int{r.RecordIdx, r.Trial}
		if r.RecordIdx < 0 || seen[key] {
			continue
		}
		seen[key] = true
		if o, ok := outputs[key]; ok && o.Record != nil {
			resp.Records = append(resp.Records, o.Record)
		} else if r.RecordIdx < len(records) {
			resp.Records = append(resp.Records, records[r.RecordIdx])
		}
	}
	return connect.NewResponse(resp), nil
}

// runTasks drives the agent through every record that has no output for this
// trial yet, as tasks in the worker's harness. Each finished task is stored
// at once, so a resumed run only repeats the tasks that were in flight.
func (ex *execution) runTasks(ctx context.Context, trial int) error {
	spec, err := ex.m.workerSpec(ex.spec)
	if err != nil {
		return err
	}
	var missing []int
	for i := range ex.records {
		if _, ok := ex.outputs[[2]int{i, trial}]; !ok {
			missing = append(missing, i)
		}
	}
	var mu sync.Mutex
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(ex.m.opts.Evaluate.Parallelism)
	for _, i := range missing {
		g.Go(func() error {
			res, err := ex.m.worker.RunTask(gctx, &pluginv1alpha1.RunTaskRequest{
				Spec: spec, Record: ex.records[i], Trial: int32(trial), RunId: ex.run.GetId(),
			}, nil)
			if err != nil {
				return fmt.Errorf("agent task %s: %w", ex.records[i].GetId(), err)
			}
			out := store.Output{RecordIdx: i, Trial: trial, Error: res.GetError()}
			if rec := res.GetRecord(); rec != nil && res.GetError() == "" {
				iso := rec.GetProvenance().GetIsolation()
				rec.Provenance = &evalsiv1alpha1.Provenance{
					Source: &evalsiv1alpha1.Provenance_Run{Run: &evalsiv1alpha1.RunProvenance{
						RunId: ex.run.GetId(), TaskId: rec.GetId(), Trial: int32(trial),
					}},
					Isolation: iso,
				}
				out.Record = rec
			} else if out.Error == "" {
				out.Error = "the worker returned no record"
			}
			mu.Lock()
			defer mu.Unlock()
			if err := ex.m.store.PutOutputs(gctx, ex.run.GetId(), []store.Output{out}); err != nil {
				return err
			}
			ex.outputs[[2]int{i, trial}] = out
			addUsage(ex.run.TargetUsage, out.Record.GetUsage())
			if err := ex.progress(gctx, 1); err != nil {
				return err
			}
			return ex.checkBudget()
		})
	}
	return g.Wait()
}
