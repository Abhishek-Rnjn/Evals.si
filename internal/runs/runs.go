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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	pluginv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/plugin/v1alpha1"
	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/auth"
	"github.com/abhishek-rnjn/evals.si/internal/authz"
	"github.com/abhishek-rnjn/evals.si/internal/config"
	"github.com/abhishek-rnjn/evals.si/internal/credentials"
	"github.com/abhishek-rnjn/evals.si/internal/datasetcatalog"
	"github.com/abhishek-rnjn/evals.si/internal/datasets"
	"github.com/abhishek-rnjn/evals.si/internal/evaluation"
	"github.com/abhishek-rnjn/evals.si/internal/objstore"
	"github.com/abhishek-rnjn/evals.si/internal/pluginhost"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

// Options configures the manager.
type Options struct {
	// Worker-visible judge settings captured in the release evaluation contract.
	Judges map[string]config.Judge
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
	// Coordinates replicas that share the database; nil for a single replica.
	Cluster Coordinator
	// Lease timing in a cluster; defaults 30s and 10s (shorter in tests).
	LeaseTTL      time.Duration
	AdoptInterval time.Duration
	// Per-project limits (concurrent and stored runs, daily tokens).
	Quotas config.Quotas
	// How long finished runs are kept (see Manager.Retain).
	Retention config.Runs
	// Records an audit event before the change it describes; nil records
	// none. Automatic deletes and archives use it, and skip the run when it
	// returns an error (strict audit mode).
	Audit func(context.Context, *evalsiv1alpha1.AuditEvent) error
	// The time, for daily quotas; time.Now when nil (tests set it).
	Now func() time.Time
}

// Coordinator connects the replicas sharing a database (see the cluster
// package). With it, a run executes on the replica that holds its lease
// (run:<id>), renewed while it runs; runs whose lease expires (their
// replica died) are adopted by another replica and resume where they were.
// Watchers and cancellations reach the executing replica over it.
type Coordinator interface {
	Owner() string
	PublishRunEvent(id string, ev *evalsiv1alpha1.WatchRunResponse)
	SubscribeRunEvents(id string) (<-chan *evalsiv1alpha1.WatchRunResponse, func(), error)
	RequestCancel(id string) error
	OnCancel(fn func(id string)) (func(), error)
}

// Manager implements evalsiv1alpha1connect.RunServiceHandler.
type Manager struct {
	store  *store.Store
	worker pluginhost.Worker
	engine *evaluation.Service
	opts   Options
	slots  chan struct{}
	log    *slog.Logger

	// admit serializes quota checks with the inserts they admit.
	admit sync.Mutex

	mu     sync.Mutex
	active map[string]*activeRun
	// Per-project concurrency slots, made on first use.
	projectSlots map[string]chan struct{}
	// Runs that finished here, by final status (for /metrics).
	finished  map[evalsiv1alpha1.RunStatus]int64
	executing int
	stopping  bool
	// Stops the cluster loops (adoption, cancel subscription).
	stopCluster func()
}

type activeRun struct {
	id        string
	cancel    context.CancelFunc
	cancelled bool
	// In a cluster: the run stopped without finishing here (shutdown, or
	// its lease went to another replica), so its state is not written.
	handoff bool
	done    chan struct{}
	subs    map[chan *evalsiv1alpha1.WatchRunResponse]bool
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
	if opts.Now == nil {
		opts.Now = time.Now
	}
	m := &Manager{
		store: st, worker: worker, engine: engine, opts: opts,
		slots:  make(chan struct{}, opts.MaxConcurrent),
		log:    opts.Logger,
		active: map[string]*activeRun{},

		projectSlots: map[string]chan struct{}{},
		finished:     map[evalsiv1alpha1.RunStatus]int64{},
	}
	if opts.Cluster != nil {
		return m, m.joinCluster()
	}
	stale, err := st.RunsWithStatus(ctx, evalsiv1alpha1.RunStatus_RUN_STATUS_PENDING, evalsiv1alpha1.RunStatus_RUN_STATUS_RUNNING)
	if err != nil {
		return nil, err
	}
	for _, run := range stale {
		run.Status = evalsiv1alpha1.RunStatus_RUN_STATUS_ERROR
		run.Error = "the server stopped while this run was in progress; resume it to continue"
		// Without it retention would age the run from CreatedAt and could
		// delete one resumed days after creation on its first pass.
		run.FinishedAt = timestamppb.New(opts.Now())
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

// Shutdown cancels every executing run and waits. A single replica marks
// them ERROR (resumable); in a cluster they are handed off: their leases are
// released and another replica adopts them.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	m.stopping = true
	for _, a := range m.active {
		if m.opts.Cluster != nil {
			a.handoff = true
		}
		a.cancel()
	}
	m.mu.Unlock()
	if m.stopCluster != nil {
		m.stopCluster()
	}
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

// checkSpecGrants checks the worker variables and the user simulator's judge
// a spec names against the project's current credential grants.
func (m *Manager) checkSpecGrants(ctx context.Context, project string, spec *evalsiv1alpha1.RunSpec) error {
	uses, err := credentials.SpecUses(spec)
	if err != nil {
		return err
	}
	if err := m.engine.Credentials().Check(ctx, project, uses); err != nil {
		return err
	}
	if sim := spec.GetHarness().GetBuiltin().GetUserSimulator(); sim != nil {
		judge := sim.GetJudge()
		if judge == "" {
			judge = m.engine.JudgeOr(spec.GetJudge())
		}
		if err := m.engine.Credentials().CheckJudge(ctx, project, judge, "spec.harness.builtin.user_simulator"); err != nil {
			return err
		}
	}
	return nil
}

// checkGrants checks a stored run against the current credential grants:
// a grant revoked after the run was created stops it before it starts new
// work, whether it is queued, resumed or adopted.
func (ex *execution) checkGrants(ctx context.Context) error {
	project := projectOr(ex.run.GetProject())
	if err := ex.m.engine.Authorize(ctx, project, ex.insts); err != nil {
		return err
	}
	return ex.m.checkSpecGrants(ctx, project, ex.spec)
}

// validate checks a spec before anything is stored, including the worker
// variables it names against the project's credential grants.
func (m *Manager) validate(ctx context.Context, project string, spec *evalsiv1alpha1.RunSpec) ([]evaluation.Instance, error) {
	if spec == nil {
		return nil, invalid("spec is required")
	}
	project = projectOr(project)
	insts, err := m.engine.BindFor(ctx, project, spec.GetEvaluators(), spec.GetJudge())
	if err != nil {
		return nil, err
	}
	if err := m.checkSpecGrants(ctx, project, spec); err != nil {
		return nil, err
	}
	if spec.GetDataset().GetSource() == nil {
		return nil, invalid("spec.dataset needs one of inline, path or uri")
	}
	if spec.GetTrials() < 0 {
		return nil, invalid("spec.trials cannot be negative")
	}
	if spec.GetSweep() != nil {
		// Each variant is validated as a run of its own.
		if err := m.validateSweep(ctx, project, spec); err != nil {
			return nil, err
		}
		if err := validateTargetOptions(spec.GetTarget(), "spec.target"); err != nil {
			return nil, err
		}
		if err := validateGates(spec); err != nil {
			return nil, err
		}
		return insts, nil
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
	if err := validateTargetOptions(spec.GetTarget(), "spec.target"); err != nil {
		return nil, err
	}
	if err := validateGates(spec); err != nil {
		return nil, err
	}
	return insts, nil
}

// defaultMaxTokens is the output limit a target gets when it sets none (the
// Python worker's default).
const defaultMaxTokens = 16000

// maxFallbacks bounds the models a run may switch between.
const maxFallbacks = 4

// validateTargetOptions checks a target's model options (and its fallbacks),
// so a bad value is refused here rather than failing every trial.
func validateTargetOptions(t *evalsiv1alpha1.Target, field string) error {
	if t == nil {
		return nil
	}
	if t.GetMaxConcurrency() < 0 || t.GetFallbackAfter() < 0 {
		return invalid("%s: max_concurrency and fallback_after cannot be negative", field)
	}
	switch t.GetApi() {
	case "", "chat-completions", "responses":
	default:
		return invalid("%s.api must be chat-completions or responses", field)
	}
	if t.GetApi() == "responses" && t.GetConnector() == "anthropic" {
		return invalid("%s.api responses is for openai-compatible targets", field)
	}
	if th := t.GetThinking(); th != nil {
		switch th.GetMode() {
		case "adaptive", "disabled":
		case "enabled":
			if th.GetBudgetTokens() <= 0 {
				return invalid("%s.thinking.budget_tokens is required when mode is enabled", field)
			}
			limit := t.GetMaxTokens()
			if limit == 0 {
				limit = defaultMaxTokens
			}
			if th.GetBudgetTokens() >= limit {
				return invalid("%s.thinking.budget_tokens must be below max_tokens (%d unless set)", field, limit)
			}
		default:
			return invalid("%s.thinking.mode must be adaptive, enabled or disabled", field)
		}
		if t.GetConnector() != "anthropic" {
			return invalid("%s.thinking is for anthropic targets", field)
		}
	}
	if err := validateH1bOptions(t, field); err != nil {
		return err
	}
	if len(t.GetFallbacks()) > maxFallbacks {
		return invalid("%s.fallbacks can hold at most %d models", field, maxFallbacks)
	}
	for i, f := range t.GetFallbacks() {
		name := fmt.Sprintf("%s.fallbacks[%d]", field, i)
		switch {
		case f.GetConnector() != "openai-compatible" && f.GetConnector() != "anthropic":
			return invalid("%s.connector must be openai-compatible or anthropic", name)
		case f.GetModel() == "":
			return invalid("%s.model is required", name)
		case f.GetConnector() == "openai-compatible" && f.GetBaseUrl() == "":
			return invalid("%s.base_url is required for openai-compatible", name)
		case len(f.GetFallbacks()) > 0:
			return invalid("%s cannot have fallbacks of its own", name)
		}
		if err := validateTargetOptions(f, name); err != nil {
			return err
		}
	}
	return nil
}

// validateH1bOptions checks a target's frontier-model options (reasoning
// handoff, token trace, server tools, stream resume) and its compaction and
// sub-agent models.
func validateH1bOptions(t *evalsiv1alpha1.Target, field string) error {
	switch t.GetReasoningHandoff() {
	case "", "drop", "summarize-as-text":
	default:
		return invalid("%s.reasoning_handoff must be drop or summarize-as-text", field)
	}
	switch t.GetTokenTraceServer() {
	case "", "vllm", "sglang":
	default:
		return invalid("%s.token_trace_server must be vllm or sglang", field)
	}
	if t.GetTokenTraceServer() != "" && !t.GetTokenTrace() {
		return invalid("%s.token_trace_server needs token_trace", field)
	}
	if t.GetTokenTrace() && (t.GetConnector() != "openai-compatible" || t.GetApi() == "responses") {
		return invalid("%s.token_trace is for openai-compatible chat-completions targets", field)
	}
	if t.GetStreamResumeAttempts() < 0 {
		return invalid("%s.stream_resume_attempts cannot be negative", field)
	}
	switch t.GetStreamResumeMode() {
	case "", "retry":
	case "prefill":
		if t.GetConnector() != "anthropic" {
			return invalid("%s.stream_resume_mode prefill is for anthropic targets", field)
		}
	default:
		return invalid("%s.stream_resume_mode must be retry or prefill", field)
	}
	if len(t.GetServerTools()) > 0 && t.GetConnector() != "anthropic" {
		return invalid("%s.server_tools are for anthropic targets", field)
	}
	for i, st := range t.GetServerTools() {
		f := st.GetFields()
		if f["type"].GetStringValue() == "" || f["name"].GetStringValue() == "" {
			return invalid("%s.server_tools[%d] needs a type and a name", field, i)
		}
	}
	roles := []struct {
		name string
		m    *evalsiv1alpha1.Target
	}{{"compaction_model", t.GetCompactionModel()}, {"subagent_model", t.GetSubagentModel()}}
	for _, r := range roles {
		m := r.m
		if m == nil {
			continue
		}
		name := field + "." + r.name
		switch {
		case m.GetConnector() != "openai-compatible" && m.GetConnector() != "anthropic":
			return invalid("%s.connector must be openai-compatible or anthropic", name)
		case m.GetModel() == "":
			return invalid("%s.model is required", name)
		case m.GetConnector() == "openai-compatible" && m.GetBaseUrl() == "":
			return invalid("%s.base_url is required for openai-compatible", name)
		case m.GetCompactionModel() != nil || m.GetSubagentModel() != nil:
			return invalid("%s cannot name compaction or sub-agent models of its own", name)
		}
		if err := validateTargetOptions(m, name); err != nil {
			return err
		}
	}
	return nil
}

// validateGates checks the parts of a spec that are read when the run
// finishes, so a bad value is refused here rather than failing the run.
func validateGates(spec *evalsiv1alpha1.RunSpec) error {
	for _, g := range spec.GetGates() {
		if g.GetMetric() == "" || (g.Min == nil && g.Max == nil) {
			return invalid("every gate needs a metric and a min or max")
		}
		// An unknown stat would otherwise be checked as the mean.
		if _, known := evalsiv1alpha1.GateStat_name[int32(g.GetStat())]; !known {
			return invalid("gate on %q has unknown stat %d", g.GetMetric(), g.GetStat())
		}
		// A NaN bound fails both comparisons, so the gate would always pass.
		for _, bound := range []*float64{g.Min, g.Max} {
			if bound != nil && (math.IsNaN(*bound) || math.IsInf(*bound, 0)) {
				return invalid("gate on %q has a bound that is not a finite number", g.GetMetric())
			}
		}
	}
	// Zero means the default; anything else must be inside (0, 1), which NaN is not.
	if level := spec.GetSummary().GetConfidenceLevel(); level != 0 && !(level > 0 && level < 1) {
		return invalid("spec.summary.confidence_level must be between 0 and 1")
	}
	return nil
}

// resolvePath keeps dataset paths inside DatasetsDir, symlinks included. On
// object storage the result is an s3:// URL, which the worker side fetches.
// project is the project reading the path ("" for none).
func (m *Manager) resolvePath(rel, project string) (string, error) {
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
	// A relative datasets_dir (./datasets) is relative to where evalsid
	// started; the worker wants absolute paths. Make it absolute before
	// resolving symlinks, so a working directory reached through a symlink
	// resolves the same way as the files inside it.
	root, err := filepath.Abs(m.opts.DatasetsDir)
	if err == nil {
		root, err = filepath.EvalSymlinks(root)
	}
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
	r, err := filepath.Rel(root, full)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(os.PathSeparator)) {
		return "", invalid("dataset %q is outside the server's datasets_dir", rel)
	}
	// Access rules check the project a promoted dataset belongs to by the
	// path as spelled; an alias (a symlink) must not lead into another
	// project's promotions.
	if to := datasets.ProjectOf(r); to != "" && to != datasets.ProjectOf(rel) && to != project {
		return "", invalid("dataset %q resolves to %q; name it by that path", rel, filepath.ToSlash(r))
	}
	return full, nil
}

// resolveURI passes hf:// through and confines importer URIs
// (scheme://path?options, for example inspect://logs/run.eval), which name
// local files, to DatasetsDir like plain paths.
func (m *Manager) resolveURI(uri, project string) (string, error) {
	scheme, rest, ok := strings.Cut(uri, "://")
	if !ok || scheme == "" {
		return "", invalid("dataset uri %q needs a scheme such as hf:// or inspect://; use path for files", uri)
	}
	if scheme == "hf" {
		return uri, nil
	}
	path, query, hasQuery := strings.Cut(rest, "?")
	full, err := m.resolvePath(path, project)
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
	case *evalsiv1alpha1.DatasetSource_Catalog:
		if len(src.GetMapping()) != 0 || src.GetLimit() < 0 {
			return nil, invalid("catalog datasets reject mapping and negative limits")
		}
		records, err = datasetcatalog.Load(ctx, m.store, s.Catalog, projectOr(project), int(src.GetLimit()))
		if err != nil {
			return nil, err
		}
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
			full, err := m.resolvePath(s.Path, projectOr(project))
			if err != nil {
				return nil, err
			}
			req.Source = &evalsiv1alpha1.DatasetSource_Path{Path: full}
		case *evalsiv1alpha1.DatasetSource_Uri:
			uri, err := m.resolveURI(s.Uri, projectOr(project))
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
	insts, err := m.validate(ctx, req.Msg.GetProject(), spec)
	if err != nil {
		return nil, err
	}
	if req.Msg.GetValidateOnly() {
		return connect.NewResponse(&evalsiv1alpha1.CreateRunResponse{}), nil
	}
	if spec.GetSweep() != nil {
		resp, err := m.createSweep(ctx, req.Msg, insts)
		if err != nil {
			return nil, err
		}
		return connect.NewResponse(resp), nil
	}
	project := projectOr(req.Msg.GetProject())
	if err := m.checkQuota(ctx, project, 1); err != nil {
		return nil, err
	}
	records, err := m.loadDataset(ctx, spec.GetDataset(), project)
	if err != nil {
		return nil, err
	}
	m.admit.Lock()
	defer m.admit.Unlock()
	// Again, with the insert: the dataset may have taken a while to load.
	if err := m.checkQuota(ctx, project, 1); err != nil {
		return nil, err
	}
	run, err := m.storeRun(ctx, req.Msg.GetName(), project, req.Msg.GetLabels(), spec, insts, records)
	if err != nil {
		return nil, err
	}
	m.start(run)
	return connect.NewResponse(&evalsiv1alpha1.CreateRunResponse{Run: run}), nil
}

// storeRun stores a validated run over a dataset snapshot; start runs it.
func (m *Manager) storeRun(ctx context.Context, name, project string, labels map[string]string, spec *evalsiv1alpha1.RunSpec, insts []evaluation.Instance, records []*evalsiv1alpha1.Record, ids ...string) (*evalsiv1alpha1.Run, error) {
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
	contract, err := m.evaluationContract(spec, insts)
	if err != nil {
		return nil, err
	}
	run.EvaluationContract = contract
	if len(ids) > 0 {
		run.Id = ids[0]
	}
	if err := m.store.CreateRun(ctx, run, records); err != nil {
		return nil, err
	}
	return run, nil
}

func (m *Manager) CreateScheduledRun(ctx context.Context, id string, request *evalsiv1alpha1.CreateRunRequest) (*evalsiv1alpha1.Run, error) {
	if existing, err := m.store.GetRun(ctx, id); err == nil {
		if existing.GetProject() != projectOr(request.GetProject()) || existing.GetName() != request.GetName() {
			return nil, connect.NewError(connect.CodeAlreadyExists, errors.New("scheduled run identity conflict"))
		}
		return existing, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	instances, err := m.validate(ctx, request.GetProject(), request.GetSpec())
	if err != nil {
		return nil, err
	}
	records, err := m.loadDataset(ctx, request.GetSpec().GetDataset(), projectOr(request.GetProject()))
	if err != nil {
		return nil, err
	}
	m.admit.Lock()
	defer m.admit.Unlock()
	if existing, err := m.store.GetRun(ctx, id); err == nil {
		return existing, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	if err := m.checkQuota(ctx, projectOr(request.GetProject()), 1); err != nil {
		return nil, err
	}
	run, err := m.storeRun(ctx, request.GetName(), projectOr(request.GetProject()), request.GetLabels(), request.GetSpec(), instances, records, id)
	if err != nil {
		if existing, getErr := m.store.GetRun(ctx, id); getErr == nil {
			return existing, nil
		}
		return nil, err
	}
	m.start(run)
	return run, nil
}

func (m *Manager) start(run *evalsiv1alpha1.Run) {
	ctx, cancel := context.WithCancel(context.Background())
	a := &activeRun{id: run.GetId(), cancel: cancel, done: make(chan struct{}), subs: map[chan *evalsiv1alpha1.WatchRunResponse]bool{}}
	m.mu.Lock()
	if _, dup := m.active[run.GetId()]; dup || m.stopping {
		m.mu.Unlock()
		cancel()
		return
	}
	m.active[run.GetId()] = a
	m.mu.Unlock()
	go func() {
		defer close(a.done)
		defer cancel()
		if m.opts.Cluster != nil {
			if !m.holdLease(ctx, a) {
				m.mu.Lock()
				delete(m.active, run.GetId())
				m.mu.Unlock()
				return
			}
		}
		// The project's slot first, so a project at its limit does not hold
		// global slots other projects could use.
		if slot := m.projectSlot(run.GetProject()); slot != nil {
			select {
			case slot <- struct{}{}:
				defer func() { <-slot }()
			case <-ctx.Done():
			}
		}
		select {
		case m.slots <- struct{}{}:
			defer func() { <-m.slots }()
		case <-ctx.Done():
		}
		m.mu.Lock()
		m.executing++
		m.mu.Unlock()
		m.execute(ctx, run.GetId(), a)
		m.mu.Lock()
		m.executing--
		m.mu.Unlock()
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
	if m.opts.Cluster != nil && !a.handoff {
		m.opts.Cluster.PublishRunEvent(a.id, ev)
	}
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
	// Tokens already counted toward the project's daily quota.
	countedJudge, countedTarget int64
	// Milliseconds of sandboxed work this run has done, and how many of them
	// are already counted toward the project's day.
	sandboxMillis  atomic.Int64
	countedSandbox int64

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
	// A listing taken before the run finished (cluster adopt) can start it
	// again; a finished run must not be re-executed.
	if terminal(run.GetStatus()) {
		return
	}
	finish := func(status evalsiv1alpha1.RunStatus, msg string) {
		m.mu.Lock()
		handoff := a.handoff
		m.mu.Unlock()
		if handoff {
			return // another replica continues it
		}
		run.Status, run.Error, run.FinishedAt = status, msg, timestamppb.Now()
		m.mu.Lock()
		m.finished[status]++
		m.mu.Unlock()
		if err := m.store.UpdateRun(context.Background(), run); err != nil {
			m.log.Error("saving run", "run", id, "err", err)
		}
		m.publishRun(a, run)
		if m.opts.OnFinished != nil {
			m.opts.OnFinished(proto.Clone(run).(*evalsiv1alpha1.Run))
		}
	}
	// A panic here would take the whole server down, and with it every other
	// run; the run fails instead.
	defer func() {
		if p := recover(); p != nil {
			m.log.Error("run panicked", "run", id, "panic", p, "stack", string(debug.Stack()))
			finish(evalsiv1alpha1.RunStatus_RUN_STATUS_ERROR, fmt.Sprintf("internal error: %v", p))
		}
	}()
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
	if run.GetEvaluationContract() != nil {
		current, contractErr := m.evaluationContract(run.GetSpec(), insts)
		if contractErr != nil || !proto.Equal(current, run.GetEvaluationContract()) {
			return nil, errors.New("evaluation contract changed since admission; create a new run")
		}
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
	if err := ex.checkGrants(ctx); err != nil {
		return nil, err
	}
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
	// What earlier attempts of this run used was counted when they ran.
	ex.countedJudge, ex.countedTarget = tokens(run.GetJudgeUsage()), tokens(run.GetTargetUsage())
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

func (ex *execution) checkBudget(ctx context.Context) error {
	if err := ex.checkQuota(ctx); err != nil {
		return err
	}
	if err := ex.checkGrants(ctx); err != nil {
		return err
	}
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
		if err := ex.checkBudget(ctx); err != nil {
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
		if r.EvalIdx >= 0 && r.EvalIdx < len(ex.insts) && evaluation.Metered(ex.insts[r.EvalIdx]) {
			ex.addSandbox(r.Result.GetDuration().AsDuration())
		}
		ex.m.publish(ex.a, &evalsiv1alpha1.WatchRunResponse{Event: &evalsiv1alpha1.WatchRunResponse_Result{Result: r.Result}})
	}
	if err := ex.progress(ctx, len(batch)); err != nil {
		return err
	}
	return ex.checkBudget(ctx)
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
	return allErrored(results)
}

// allErrored fails a run in which no evaluation produced an outcome: with
// every result an error (an unreachable sandbox, a bad key), its summaries
// are empty and a run without gates would otherwise look like a success.
func allErrored(results []*evalsiv1alpha1.EvaluationResult) error {
	if len(results) == 0 {
		return nil
	}
	for _, r := range results {
		if r.GetOutcome() != evalsiv1alpha1.Outcome_OUTCOME_ERROR {
			return nil
		}
	}
	return fmt.Errorf("all %d evaluations failed; the first: %s", len(results), results[0].GetReason())
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
		case evalsiv1alpha1.GateStat_GATE_STAT_ERROR_RATE:
			stat = "error_rate"
			if ran := s.GetN() + s.GetErrors(); ran > 0 {
				value = proto.Float64(float64(s.GetErrors()) / float64(ran))
			}
		default:
			value = s.Mean
		}
		res.Value = value
		switch {
		case value == nil:
			res.Reason = "metric has no " + stat
		case math.IsNaN(*value) || math.IsInf(*value, 0):
			// NaN fails every comparison below, so it would otherwise pass.
			res.Reason = fmt.Sprintf("%s is %v, not a finite number", stat, *value)
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
	visible := runs[:0]
	for _, r := range runs {
		if authz.Can(ctx, "runs.read", r.GetProject(), authz.RunResourceFor(ctx, r)) {
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
		if m.opts.Cluster != nil && !terminal(run.GetStatus()) {
			return m.watchRemote(ctx, id, req.Msg.GetIncludeResults(), stream.Send)
		}
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
	} else if m.opts.Cluster != nil {
		run, err := m.cancelRemote(ctx, id)
		if err != nil {
			return nil, err
		}
		return connect.NewResponse(&evalsiv1alpha1.CancelRunResponse{Run: run}), nil
	}
	run, err := m.get(ctx, id)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&evalsiv1alpha1.CancelRunResponse{Run: run}), nil
}

// ResumeRun continues an errored or cancelled run.
func (m *Manager) ResumeRun(ctx context.Context, req *connect.Request[evalsiv1alpha1.ResumeRunRequest]) (*connect.Response[evalsiv1alpha1.ResumeRunResponse], error) {
	m.admit.Lock()
	defer m.admit.Unlock()
	run, err := m.get(ctx, req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	if err := errArchived(run); err != nil {
		return nil, err
	}
	if run.GetImported() {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("run %s was imported: it was evaluated elsewhere, so there is nothing here to resume", run.GetId()))
	}
	m.mu.Lock()
	_, running := m.active[run.GetId()]
	m.mu.Unlock()
	if m.opts.Cluster != nil && !running {
		holder, err := m.store.LeaseHolder(ctx, leaseName(run.GetId()))
		if err != nil {
			return nil, err
		}
		running = holder != ""
	}
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

func (m *Manager) DeleteRun(ctx context.Context, req *connect.Request[evalsiv1alpha1.DeleteRunRequest]) (*connect.Response[evalsiv1alpha1.DeleteRunResponse], error) {
	m.admit.Lock()
	defer m.admit.Unlock()
	m.mu.Lock()
	_, active := m.active[req.Msg.GetId()]
	m.mu.Unlock()
	if active {
		return nil, connect.NewError(connect.CodeFailedPrecondition, store.ErrRunActive)
	}
	run, err := m.get(ctx, req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	resource, err := json.Marshal(authz.RunResourceFor(ctx, run))
	if err != nil {
		return nil, err
	}
	if err := m.store.DeleteRun(ctx, req.Msg.GetId(), resource); err != nil {
		if errors.Is(err, store.ErrRunActive) {
			return nil, connect.NewError(connect.CodeFailedPrecondition, err)
		}
		if errors.Is(err, store.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, err
	}
	return connect.NewResponse(&evalsiv1alpha1.DeleteRunResponse{}), nil
}

// ArchiveRun drops a finished run's records, outputs, results and webhook
// deliveries and keeps its summary (see Store.ArchiveRun).
func (m *Manager) ArchiveRun(ctx context.Context, req *connect.Request[evalsiv1alpha1.ArchiveRunRequest]) (*connect.Response[evalsiv1alpha1.ArchiveRunResponse], error) {
	run, _, err := m.archive(ctx, req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&evalsiv1alpha1.ArchiveRunResponse{Run: run}), nil
}

// archive archives one run unless it is active here or on another replica.
// It reports whether this call archived it (false: it already was).
func (m *Manager) archive(ctx context.Context, id string) (*evalsiv1alpha1.Run, bool, error) {
	m.admit.Lock()
	defer m.admit.Unlock()
	m.mu.Lock()
	_, active := m.active[id]
	m.mu.Unlock()
	if active {
		return nil, false, connect.NewError(connect.CodeFailedPrecondition, store.ErrRunActive)
	}
	before, err := m.get(ctx, id)
	if err != nil {
		return nil, false, err
	}
	run, err := m.store.ArchiveRun(ctx, id, m.opts.Now())
	switch {
	case errors.Is(err, store.ErrRunActive):
		return nil, false, connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, store.ErrNotFound):
		return nil, false, connect.NewError(connect.CodeNotFound, err)
	case err != nil:
		return nil, false, err
	}
	return run, before.GetArchivedAt() == nil, nil
}

// errArchived is the answer to a request that needs what archiving removed.
func errArchived(run *evalsiv1alpha1.Run) error {
	if run.GetArchivedAt() == nil {
		return nil
	}
	return connect.NewError(connect.CodeFailedPrecondition,
		fmt.Errorf("run %s was archived at %s: its records and results are gone; its summary remains", run.GetId(), run.GetArchivedAt().AsTime().Format(time.RFC3339)))
}

// ListRunResults pages through a run's results with the graded records.
func (m *Manager) ListRunResults(ctx context.Context, req *connect.Request[evalsiv1alpha1.ListRunResultsRequest]) (*connect.Response[evalsiv1alpha1.ListRunResultsResponse], error) {
	id := req.Msg.GetRunId()
	run, err := m.get(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := errArchived(run); err != nil {
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
		evidence := &evalsiv1alpha1.RunRecordEvidence{RecordId: r.Result.GetRecordId(), Trial: int32(r.Trial)}
		if o, ok := outputs[key]; ok && o.Record != nil {
			resp.Records = append(resp.Records, o.Record)
			evidence.Record, evidence.OutputStored, evidence.OutputError = o.Record, true, o.Error
		} else if r.RecordIdx < len(records) {
			resp.Records = append(resp.Records, records[r.RecordIdx])
			evidence.Record = records[r.RecordIdx]
			if o, ok := outputs[key]; ok {
				evidence.OutputStored, evidence.OutputError = true, o.Error
			}
		}
		resp.RecordsByTrial = append(resp.RecordsByTrial, evidence)
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
			started := time.Now()
			res, err := ex.m.worker.RunTask(gctx, &pluginv1alpha1.RunTaskRequest{
				Spec: spec, Record: ex.records[i], Trial: int32(trial), RunId: ex.run.GetId(),
			}, nil)
			// An agent task runs in a sandbox from start to end, whether it succeeds or not.
			ex.addSandbox(time.Since(started))
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
			return ex.checkBudget(gctx)
		})
	}
	return g.Wait()
}

// WriteMetrics writes the run metrics of this replica.
func (m *Manager) WriteMetrics(w io.Writer) {
	m.mu.Lock()
	active, executing := len(m.active), m.executing
	finished := make(map[evalsiv1alpha1.RunStatus]int64, len(m.finished))
	for k, v := range m.finished {
		finished[k] = v
	}
	m.mu.Unlock()
	fmt.Fprintf(w, "# HELP evalsi_runs_active Runs on this replica, executing or waiting for a slot.\n# TYPE evalsi_runs_active gauge\nevalsi_runs_active %d\n", active)
	fmt.Fprintf(w, "# HELP evalsi_runs_executing Runs executing on this replica.\n# TYPE evalsi_runs_executing gauge\nevalsi_runs_executing %d\n", executing)
	fmt.Fprintf(w, "# HELP evalsi_runs_finished_total Runs that finished on this replica, by status.\n# TYPE evalsi_runs_finished_total counter\n")
	for _, st := range []evalsiv1alpha1.RunStatus{
		evalsiv1alpha1.RunStatus_RUN_STATUS_SUCCEEDED, evalsiv1alpha1.RunStatus_RUN_STATUS_FAILED,
		evalsiv1alpha1.RunStatus_RUN_STATUS_ERROR, evalsiv1alpha1.RunStatus_RUN_STATUS_CANCELLED,
	} {
		name := strings.ToLower(strings.TrimPrefix(st.String(), "RUN_STATUS_"))
		fmt.Fprintf(w, "evalsi_runs_finished_total{status=%q} %d\n", name, finished[st])
	}
}
