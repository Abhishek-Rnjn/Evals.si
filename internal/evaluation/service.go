// Package evaluation implements the Score door: EvaluationService and
// CatalogService. Records are sent to evaluator workers in batches, results
// come back in a stable order, and summaries are computed here.
package evaluation

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"

	"connectrpc.com/connect"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/types/known/structpb"

	pluginv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/plugin/v1alpha1"
	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/catalog"
	"github.com/abhishek-rnjn/evals.si/internal/config"
	"github.com/abhishek-rnjn/evals.si/internal/pluginhost"
)

// Service implements evalsiv1alpha1connect.EvaluationServiceHandler and CatalogServiceHandler.
type Service struct {
	worker       pluginhost.Worker
	catalog      *catalog.Catalog
	judges       []string
	defaultJudge string
	opts         config.Evaluate
}

// New builds a service over a worker whose evaluators are already described.
func New(worker pluginhost.Worker, cat *catalog.Catalog, judges []string, defaultJudge string, opts config.Evaluate) *Service {
	judges = append([]string(nil), judges...)
	sort.Strings(judges)
	return &Service{worker: worker, catalog: cat, judges: judges, defaultJudge: defaultJudge, opts: opts}
}

// RunsCode reports whether any referenced evaluator executes code, which
// access rules see as resource.runs_code. Unknown references count as not
// running code; binding rejects them anyway.
func (s *Service) RunsCode(refs []*evalsiv1alpha1.EvaluatorRef) bool {
	for _, r := range refs {
		m, err := s.catalog.Resolve(r.GetRef())
		if err != nil {
			continue
		}
		if m.GetRequires().GetIsolation() > evalsiv1alpha1.IsolationLevel_ISOLATION_LEVEL_NONE ||
			m.GetScheduling().GetPool() == "sandbox" {
			return true
		}
	}
	return false
}

// Instance is one evaluator as used in a request: resolved, aliased, with params.
type Instance struct {
	Name     string
	Manifest *evalsiv1alpha1.EvaluatorManifest
	Params   *structpb.Struct
	Judge    string
}

// Dataset reports whether the evaluator runs once over all records.
func (in Instance) Dataset() bool {
	return in.Manifest.GetScope() == evalsiv1alpha1.Scope_SCOPE_DATASET
}

func invalid(format string, args ...any) error {
	return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(format, args...))
}

// Bind resolves evaluator refs, checks params and assigns judges.
func (s *Service) Bind(refs []*evalsiv1alpha1.EvaluatorRef, judge string) ([]Instance, error) {
	if len(refs) == 0 {
		return nil, invalid("at least one evaluator is required")
	}
	if judge == "" {
		judge = s.defaultJudge
	}
	seen := map[string]bool{}
	var out []Instance
	for _, ref := range refs {
		m, err := s.catalog.Resolve(ref.GetRef())
		if err != nil {
			return nil, invalid("%v", err)
		}
		name := ref.GetName()
		if name == "" {
			name = catalog.ShortName(m.GetName())
		}
		if seen[name] {
			return nil, invalid("evaluator %q is used more than once; give each use a distinct name", name)
		}
		seen[name] = true
		if err := checkParams(m, ref.GetParams()); err != nil {
			return nil, err
		}
		in := Instance{Name: name, Manifest: m, Params: ref.GetParams()}
		if m.GetRequires().GetJudge() {
			if judge == "" {
				return nil, invalid("%s needs a judge; name one in the request or set default_judge on the server", name)
			}
			if !s.hasJudge(judge) {
				return nil, invalid("unknown judge %q; configured: %v", judge, s.judges)
			}
			in.Judge = judge
		}
		out = append(out, in)
	}
	return out, nil
}

func (s *Service) hasJudge(name string) bool {
	i := sort.SearchStrings(s.judges, name)
	return i < len(s.judges) && s.judges[i] == name
}

// checkParams validates params against the manifest's params schema
// (property names and required keys) before any work is sent to a worker.
func checkParams(m *evalsiv1alpha1.EvaluatorManifest, params *structpb.Struct) error {
	schema := m.GetParamsSchema().AsMap()
	props, _ := schema["properties"].(map[string]any)
	for key := range params.GetFields() {
		if _, ok := props[key]; !ok {
			names := make([]string, 0, len(props))
			for k := range props {
				names = append(names, k)
			}
			sort.Strings(names)
			return invalid("%s got unknown param %q (known: %v)", m.GetName(), key, names)
		}
	}
	required, _ := schema["required"].([]any)
	for _, r := range required {
		if _, ok := params.GetFields()[fmt.Sprint(r)]; !ok {
			return invalid("%s requires param %q", m.GetName(), r)
		}
	}
	return nil
}

// NormalizeIDs gives records without an id their position, as the Python SDK
// does, and rejects duplicates. offset is the position of records[0].
func NormalizeIDs(records []*evalsiv1alpha1.Record, offset int, seen map[string]bool) error {
	for i, r := range records {
		if r.GetId() == "" {
			r.Id = strconv.Itoa(offset + i)
		}
		if seen[r.GetId()] {
			return invalid("duplicate record id %q", r.GetId())
		}
		seen[r.GetId()] = true
	}
	return nil
}

// workerError keeps the worker's status code for request problems and wraps
// everything else as Unavailable/Internal.
func workerError(name string, err error) error {
	code := connect.CodeOf(err)
	switch code {
	case connect.CodeInvalidArgument, connect.CodeFailedPrecondition, connect.CodeCanceled, connect.CodeDeadlineExceeded:
	case connect.CodeUnavailable, connect.CodeUnknown:
		code = connect.CodeUnavailable
	default:
		code = connect.CodeInternal
	}
	var ce *connect.Error
	msg := err.Error()
	if errors.As(err, &ce) {
		msg = ce.Message()
	}
	return connect.NewError(code, fmt.Errorf("evaluator %s: %s", name, msg))
}

// RunRecords runs every record-scope Instance over records and returns results
// ordered by record, then Instance.
func (s *Service) RunRecords(ctx context.Context, insts []Instance, records []*evalsiv1alpha1.Record) ([]*evalsiv1alpha1.EvaluationResult, error) {
	var recordLevel []Instance
	for _, in := range insts {
		if !in.Dataset() {
			recordLevel = append(recordLevel, in)
		}
	}
	slots := make([][]*evalsiv1alpha1.EvaluationResult, len(records))
	for i := range slots {
		slots[i] = make([]*evalsiv1alpha1.EvaluationResult, len(recordLevel))
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(s.opts.Parallelism)
	size := s.opts.BatchSize
	for ii, in := range recordLevel {
		for start := 0; start < len(records); start += size {
			end := min(start+size, len(records))
			g.Go(func() error {
				resp, err := s.worker.Evaluate(gctx, &pluginv1alpha1.EvaluateRequest{
					BatchId:   fmt.Sprintf("%s:%d", in.Name, start),
					Evaluator: in.Manifest.GetName(),
					Params:    in.Params,
					Records:   records[start:end],
					Judge:     in.Judge,
				})
				if err != nil {
					return workerError(in.Name, err)
				}
				if len(resp.GetResults()) != end-start {
					return connect.NewError(connect.CodeInternal, fmt.Errorf(
						"evaluator %s: worker returned %d results for %d records", in.Name, len(resp.GetResults()), end-start))
				}
				for k, r := range resp.GetResults() {
					r.Evaluator = in.Name
					slots[start+k][ii] = r
				}
				return nil
			})
		}
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	out := make([]*evalsiv1alpha1.EvaluationResult, 0, len(records)*len(recordLevel))
	for _, row := range slots {
		out = append(out, row...)
	}
	return out, nil
}

func eligible(req *evalsiv1alpha1.Requirements, r *evalsiv1alpha1.Record) bool {
	return (!req.GetInput() || r.GetInput() != nil) &&
		(!req.GetOutput() || r.GetOutput() != nil) &&
		(!req.GetReference() || r.GetReference() != nil) &&
		(!req.GetContext() || len(r.GetContext()) > 0) &&
		(!req.GetTrajectory() || r.GetTrajectory() != nil)
}

// RunDataset runs the dataset-scope instances once each over all eligible records.
func (s *Service) RunDataset(ctx context.Context, insts []Instance, records []*evalsiv1alpha1.Record) ([]*evalsiv1alpha1.EvaluationResult, error) {
	var out []*evalsiv1alpha1.EvaluationResult
	for _, in := range insts {
		if !in.Dataset() {
			continue
		}
		var use []*evalsiv1alpha1.Record
		for _, r := range records {
			if eligible(in.Manifest.GetRequires(), r) {
				use = append(use, r)
			}
		}
		result := &evalsiv1alpha1.EvaluationResult{
			Evaluator:    in.Name,
			EvaluatorRef: in.Manifest.GetName() + "@" + in.Manifest.GetVersion(),
		}
		resp, err := s.worker.Reduce(ctx, &pluginv1alpha1.ReduceRequest{
			Evaluator: in.Manifest.GetName(),
			Params:    in.Params,
			Records:   use,
			Judge:     in.Judge,
		})
		switch {
		case err != nil && (connect.CodeOf(err) == connect.CodeInvalidArgument || connect.CodeOf(err) == connect.CodeFailedPrecondition):
			return nil, workerError(in.Name, err)
		case err != nil:
			result.Outcome = evalsiv1alpha1.Outcome_OUTCOME_ERROR
			result.Reason = err.Error()
		case len(resp.GetScores()) == 0:
			result.Outcome = evalsiv1alpha1.Outcome_OUTCOME_SKIPPED
			result.Reason = "evaluator returned no scores"
		default:
			result.Outcome = evalsiv1alpha1.Outcome_OUTCOME_SCORED
			result.Scores = resp.GetScores()
		}
		if missing := len(records) - len(use); missing > 0 && result.Reason == "" {
			result.Reason = fmt.Sprintf("%d records lacked required fields", missing)
		}
		out = append(out, result)
	}
	return out, nil
}

// Evaluate scores a batch of records synchronously.
func (s *Service) Evaluate(ctx context.Context, req *connect.Request[evalsiv1alpha1.EvaluateRequest]) (*connect.Response[evalsiv1alpha1.EvaluateResponse], error) {
	msg := req.Msg
	if n := len(msg.GetRecords()); n > s.opts.MaxRecords {
		return nil, invalid("%d records exceed this server's limit of %d for Evaluate; use EvaluateStream", n, s.opts.MaxRecords)
	}
	insts, err := s.Bind(msg.GetEvaluators(), msg.GetJudge())
	if err != nil {
		return nil, err
	}
	if err := NormalizeIDs(msg.GetRecords(), 0, map[string]bool{}); err != nil {
		return nil, err
	}
	results, err := s.RunRecords(ctx, insts, msg.GetRecords())
	if err != nil {
		return nil, err
	}
	dataset, err := s.RunDataset(ctx, insts, msg.GetRecords())
	if err != nil {
		return nil, err
	}
	results = append(results, dataset...)
	summaries, err := Summarize(insts, results, msg.GetRecords(), msg.GetSummary(), 1)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&evalsiv1alpha1.EvaluateResponse{Results: results, Summaries: summaries}), nil
}

// EvaluateStream scores records as they arrive: one config message, then
// records. Results stream back batch by batch, then the summaries.
func (s *Service) EvaluateStream(ctx context.Context, stream *connect.BidiStream[evalsiv1alpha1.EvaluateStreamRequest, evalsiv1alpha1.EvaluateStreamResponse]) error {
	first, err := stream.Receive()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return invalid("the stream must start with a config message")
		}
		return err
	}
	cfg := first.GetConfig()
	if cfg == nil {
		return invalid("the stream must start with a config message")
	}
	insts, err := s.Bind(cfg.GetEvaluators(), cfg.GetJudge())
	if err != nil {
		return err
	}
	var all, pending []*evalsiv1alpha1.Record
	var results []*evalsiv1alpha1.EvaluationResult
	seen := map[string]bool{}
	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		if err := NormalizeIDs(pending, len(all), seen); err != nil {
			return err
		}
		batch, err := s.RunRecords(ctx, insts, pending)
		if err != nil {
			return err
		}
		for _, r := range batch {
			if err := stream.Send(&evalsiv1alpha1.EvaluateStreamResponse{
				Message: &evalsiv1alpha1.EvaluateStreamResponse_Result{Result: r},
			}); err != nil {
				return err
			}
		}
		results = append(results, batch...)
		all = append(all, pending...)
		pending = nil
		return nil
	}
	window := s.opts.BatchSize * s.opts.Parallelism
	for {
		msg, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		record := msg.GetRecord()
		if record == nil {
			return invalid("only the first stream message may carry config")
		}
		pending = append(pending, record)
		if len(pending) >= window {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := flush(); err != nil {
		return err
	}
	dataset, err := s.RunDataset(ctx, insts, all)
	if err != nil {
		return err
	}
	for _, r := range dataset {
		if err := stream.Send(&evalsiv1alpha1.EvaluateStreamResponse{
			Message: &evalsiv1alpha1.EvaluateStreamResponse_Result{Result: r},
		}); err != nil {
			return err
		}
	}
	summaries, err := Summarize(insts, append(results, dataset...), all, cfg.GetSummary(), 1)
	if err != nil {
		return err
	}
	return stream.Send(&evalsiv1alpha1.EvaluateStreamResponse{
		Message: &evalsiv1alpha1.EvaluateStreamResponse_Summaries{
			Summaries: &evalsiv1alpha1.MetricSummaries{Summaries: summaries},
		},
	})
}

// ListEvaluators implements CatalogService.
func (s *Service) ListEvaluators(context.Context, *connect.Request[evalsiv1alpha1.ListEvaluatorsRequest]) (*connect.Response[evalsiv1alpha1.ListEvaluatorsResponse], error) {
	return connect.NewResponse(&evalsiv1alpha1.ListEvaluatorsResponse{
		Evaluators:   s.catalog.Manifests(),
		Judges:       s.judges,
		DefaultJudge: s.defaultJudge,
	}), nil
}
