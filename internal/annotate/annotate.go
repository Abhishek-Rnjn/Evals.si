// Package annotate is human evaluation: annotation queues of records that
// people score against a rubric, and statistics over their answers,
// including agreement between annotators and with an evaluator's scores
// on the same records (judge calibration).
package annotate

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/auth"
	"github.com/abhishek-rnjn/evals.si/internal/authz"
	"github.com/abhishek-rnjn/evals.si/internal/runs"
	"github.com/abhishek-rnjn/evals.si/internal/stats"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

// Limits on what a request may ask for.
const (
	MaxQuestions          = 32
	MaxAnnotationsPerItem = 10
	MaxItemsPerAdd        = 10000
	DefaultLease          = 30 * time.Minute
	MaxLease              = 24 * time.Hour
)

// Service implements AnnotationService.
type Service struct {
	store *store.Store
	runs  *runs.Manager
	now   func() time.Time
}

// New makes the service.
func New(st *store.Store, rm *runs.Manager) *Service {
	return &Service{store: st, runs: rm, now: time.Now}
}

var nameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func invalid(format string, args ...any) error {
	return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(format, args...))
}

func notFound(what string) error {
	return connect.NewError(connect.CodeNotFound, fmt.Errorf("%s not found", what))
}

func project(p string) string {
	if p == "" {
		return authz.DefaultProject
	}
	return p
}

func annotator(ctx context.Context) string {
	if p := auth.PrincipalFrom(ctx); p != nil {
		return p.ID()
	}
	return "anonymous"
}

// ValidateQueue checks a queue's rubric (also used by admission).
func ValidateQueue(q *evalsiv1alpha1.AnnotationQueue) error {
	if !nameRE.MatchString(q.GetName()) {
		return fmt.Errorf("queue name %q must be lowercase letters, digits and '-'", q.GetName())
	}
	if n := len(q.GetQuestions()); n == 0 || n > MaxQuestions {
		return fmt.Errorf("a queue needs 1 to %d questions", MaxQuestions)
	}
	if n := q.GetAnnotationsPerItem(); n < 0 || n > MaxAnnotationsPerItem {
		return fmt.Errorf("annotations_per_item must be between 1 and %d", MaxAnnotationsPerItem)
	}
	seen := map[string]bool{}
	for _, qu := range q.GetQuestions() {
		name := qu.GetName()
		if !nameRE.MatchString(name) {
			return fmt.Errorf("question name %q must be lowercase letters, digits and '-'", name)
		}
		if seen[name] {
			return fmt.Errorf("question %q appears twice", name)
		}
		seen[name] = true
		switch qu.GetKind() {
		case evalsiv1alpha1.QuestionKind_QUESTION_KIND_PASS_FAIL, evalsiv1alpha1.QuestionKind_QUESTION_KIND_TEXT:
		case evalsiv1alpha1.QuestionKind_QUESTION_KIND_SCORE:
			if !(qu.GetMax() > qu.GetMin()) {
				return fmt.Errorf("question %q: a score needs max > min", name)
			}
		case evalsiv1alpha1.QuestionKind_QUESTION_KIND_LABEL:
			if len(qu.GetOptions()) < 2 {
				return fmt.Errorf("question %q: a label needs at least two options", name)
			}
			if len(slices.Compact(slices.Sorted(slices.Values(qu.GetOptions())))) != len(qu.GetOptions()) {
				return fmt.Errorf("question %q: options must be distinct", name)
			}
		default:
			return fmt.Errorf("question %q: kind is required (pass_fail, score, label or text)", name)
		}
		if qu.GetCompareMetric() != "" {
			switch qu.GetKind() {
			case evalsiv1alpha1.QuestionKind_QUESTION_KIND_PASS_FAIL, evalsiv1alpha1.QuestionKind_QUESTION_KIND_SCORE:
			default:
				return fmt.Errorf("question %q: compare_metric needs a pass_fail or score question", name)
			}
		}
	}
	return nil
}

func need(q *evalsiv1alpha1.AnnotationQueue) int {
	if n := int(q.GetAnnotationsPerItem()); n > 0 {
		return n
	}
	return 1
}

func (s *Service) queue(ctx context.Context, p, name string) (*evalsiv1alpha1.AnnotationQueue, error) {
	q, err := s.store.GetQueue(ctx, project(p), name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFound("queue " + name)
	}
	return q, err
}

// CreateQueue stores a new queue.
func (s *Service) CreateQueue(ctx context.Context, req *connect.Request[evalsiv1alpha1.CreateQueueRequest]) (*connect.Response[evalsiv1alpha1.CreateQueueResponse], error) {
	q := proto.Clone(req.Msg.GetQueue()).(*evalsiv1alpha1.AnnotationQueue)
	if q == nil {
		return nil, invalid("queue is required")
	}
	q.Project = project(q.GetProject())
	if err := ValidateQueue(q); err != nil {
		return nil, invalid("%v", err)
	}
	if q.GetAnnotationsPerItem() == 0 {
		q.AnnotationsPerItem = 1
	}
	q.CreatedAt = timestamppb.New(s.now())
	q.CreatedBy = annotator(ctx)
	if err := s.store.CreateQueue(ctx, q); err != nil {
		if errors.Is(err, store.ErrExists) {
			return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("queue %s already exists in %s", q.GetName(), q.GetProject()))
		}
		return nil, err
	}
	return connect.NewResponse(&evalsiv1alpha1.CreateQueueResponse{Queue: q}), nil
}

// ListQueues lists a project's queues.
func (s *Service) ListQueues(ctx context.Context, req *connect.Request[evalsiv1alpha1.ListQueuesRequest]) (*connect.Response[evalsiv1alpha1.ListQueuesResponse], error) {
	qs, err := s.store.ListQueues(ctx, project(req.Msg.GetProject()))
	if err != nil {
		return nil, err
	}
	// Each queue is checked by its own name and labels, so a rule hiding
	// one queue hides it here too.
	visible := qs[:0]
	for _, q := range qs {
		if authz.Can(ctx, "annotations.read", q.GetProject(), authz.QueueResource(q.GetName(), q.GetLabels())) {
			visible = append(visible, q)
		}
	}
	return connect.NewResponse(&evalsiv1alpha1.ListQueuesResponse{Queues: visible}), nil
}

// GetQueue returns a queue.
func (s *Service) GetQueue(ctx context.Context, req *connect.Request[evalsiv1alpha1.GetQueueRequest]) (*connect.Response[evalsiv1alpha1.GetQueueResponse], error) {
	q, err := s.queue(ctx, req.Msg.GetProject(), req.Msg.GetName())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&evalsiv1alpha1.GetQueueResponse{Queue: q}), nil
}

// DeleteQueue removes a queue, its items and its annotations.
func (s *Service) DeleteQueue(ctx context.Context, req *connect.Request[evalsiv1alpha1.DeleteQueueRequest]) (*connect.Response[evalsiv1alpha1.DeleteQueueResponse], error) {
	err := s.store.DeleteQueue(ctx, project(req.Msg.GetProject()), req.Msg.GetName())
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFound("queue " + req.Msg.GetName())
	}
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&evalsiv1alpha1.DeleteQueueResponse{}), nil
}

// AddItems adds a run's matching records, or inline records, to a queue.
func (s *Service) AddItems(ctx context.Context, req *connect.Request[evalsiv1alpha1.AddItemsRequest]) (*connect.Response[evalsiv1alpha1.AddItemsResponse], error) {
	msg := req.Msg
	q, err := s.queue(ctx, msg.GetProject(), msg.GetQueue())
	if err != nil {
		return nil, err
	}
	limit := int(msg.GetLimit())
	if limit <= 0 || limit > MaxItemsPerAdd {
		limit = MaxItemsPerAdd
	}
	var items []*evalsiv1alpha1.AnnotationItem
	switch src := msg.GetSource().(type) {
	case *evalsiv1alpha1.AddItemsRequest_Run:
		run, err := s.runs.Run(ctx, src.Run.GetRunId())
		if err != nil {
			return nil, err
		}
		if run.GetProject() != q.GetProject() {
			return nil, invalid("run %s is in project %s, the queue in %s", run.GetId(), run.GetProject(), q.GetProject())
		}
		matches, err := s.runs.MatchRecords(ctx, run, src.Run.GetWhen(), src.Run.GetAllTrials())
		if err != nil {
			return nil, err
		}
		for _, m := range matches {
			rec := m.Record
			if m.Produced != nil {
				rec = m.Produced
			}
			items = append(items, &evalsiv1alpha1.AnnotationItem{
				Record:    proto.Clone(rec).(*evalsiv1alpha1.Record),
				Source:    &evalsiv1alpha1.ItemSource{RunId: run.GetId(), Trial: int32(m.Trial)},
				RunScores: m.Scores,
				CreatedAt: timestamppb.New(s.now()),
			})
		}
	case *evalsiv1alpha1.AddItemsRequest_Records:
		for i, rec := range src.Records.GetRecords() {
			r := proto.Clone(rec).(*evalsiv1alpha1.Record)
			if r.GetId() == "" {
				r.Id = strconv.Itoa(i)
			}
			items = append(items, &evalsiv1alpha1.AnnotationItem{Record: r, CreatedAt: timestamppb.New(s.now())})
		}
	default:
		return nil, invalid("a source is required: a run or inline records")
	}
	if len(items) > limit {
		items = items[:limit]
	}
	if err := s.store.AddItems(ctx, q.GetProject(), q.GetName(), items); err != nil {
		return nil, err
	}
	return connect.NewResponse(&evalsiv1alpha1.AddItemsResponse{Added: int64(len(items))}), nil
}

// NextItem claims the caller's next item.
func (s *Service) NextItem(ctx context.Context, req *connect.Request[evalsiv1alpha1.NextItemRequest]) (*connect.Response[evalsiv1alpha1.NextItemResponse], error) {
	q, err := s.queue(ctx, req.Msg.GetProject(), req.Msg.GetQueue())
	if err != nil {
		return nil, err
	}
	lease := DefaultLease
	if sec := req.Msg.GetLeaseSeconds(); sec > 0 {
		lease = min(time.Duration(sec)*time.Second, MaxLease)
	}
	it, remaining, err := s.store.ClaimItem(ctx, q.GetProject(), q.GetName(), annotator(ctx), need(q), lease, s.now())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&evalsiv1alpha1.NextItemResponse{Item: it, Remaining: remaining}), nil
}

// checkAnswers validates answers against the rubric.
func checkAnswers(q *evalsiv1alpha1.AnnotationQueue, answers []*evalsiv1alpha1.Answer) error {
	byName := map[string]*evalsiv1alpha1.Question{}
	for _, qu := range q.GetQuestions() {
		byName[qu.GetName()] = qu
	}
	got := map[string]bool{}
	for _, a := range answers {
		qu, ok := byName[a.GetQuestion()]
		if !ok {
			return fmt.Errorf("no question %q in queue %s", a.GetQuestion(), q.GetName())
		}
		if got[a.GetQuestion()] {
			return fmt.Errorf("question %q answered twice", a.GetQuestion())
		}
		got[a.GetQuestion()] = true
		switch qu.GetKind() {
		case evalsiv1alpha1.QuestionKind_QUESTION_KIND_PASS_FAIL:
			if _, ok := a.GetValue().(*evalsiv1alpha1.Answer_Passed); !ok {
				return fmt.Errorf("question %q is pass/fail: answer passed", qu.GetName())
			}
		case evalsiv1alpha1.QuestionKind_QUESTION_KIND_SCORE:
			v, ok := a.GetValue().(*evalsiv1alpha1.Answer_Number)
			if !ok || math.IsNaN(v.Number) || v.Number < qu.GetMin() || v.Number > qu.GetMax() {
				return fmt.Errorf("question %q needs a number between %g and %g", qu.GetName(), qu.GetMin(), qu.GetMax())
			}
		case evalsiv1alpha1.QuestionKind_QUESTION_KIND_LABEL:
			v, ok := a.GetValue().(*evalsiv1alpha1.Answer_Label)
			if !ok || !slices.Contains(qu.GetOptions(), v.Label) {
				return fmt.Errorf("question %q needs one of %v", qu.GetName(), qu.GetOptions())
			}
		case evalsiv1alpha1.QuestionKind_QUESTION_KIND_TEXT:
			if _, ok := a.GetValue().(*evalsiv1alpha1.Answer_Text); !ok {
				return fmt.Errorf("question %q needs text", qu.GetName())
			}
		}
	}
	for _, qu := range q.GetQuestions() {
		if !qu.GetOptional() && !got[qu.GetName()] {
			return fmt.Errorf("question %q is required", qu.GetName())
		}
	}
	return nil
}

// SubmitAnnotation records the caller's answers (or skip) for an item.
func (s *Service) SubmitAnnotation(ctx context.Context, req *connect.Request[evalsiv1alpha1.SubmitAnnotationRequest]) (*connect.Response[evalsiv1alpha1.SubmitAnnotationResponse], error) {
	msg := req.Msg
	q, err := s.queue(ctx, msg.GetProject(), msg.GetQueue())
	if err != nil {
		return nil, err
	}
	if _, err := s.store.GetItem(ctx, q.GetProject(), q.GetName(), msg.GetItemId()); errors.Is(err, store.ErrNotFound) {
		return nil, notFound("item " + msg.GetItemId())
	} else if err != nil {
		return nil, err
	}
	if !msg.GetSkip() {
		if err := checkAnswers(q, msg.GetAnswers()); err != nil {
			return nil, invalid("%v", err)
		}
	}
	a := &evalsiv1alpha1.Annotation{
		ItemId: msg.GetItemId(), Queue: q.GetName(), Annotator: annotator(ctx),
		Answers: msg.GetAnswers(), Comment: msg.GetComment(), Skipped: msg.GetSkip(),
		CreatedAt: timestamppb.New(s.now()),
	}
	if a.Skipped {
		a.Answers = nil
	}
	if _, err := s.store.SaveAnnotation(ctx, q.GetProject(), a, need(q)); err != nil {
		return nil, err
	}
	return connect.NewResponse(&evalsiv1alpha1.SubmitAnnotationResponse{Annotation: a}), nil
}

// ListAnnotations pages through a queue's annotations, with their items.
func (s *Service) ListAnnotations(ctx context.Context, req *connect.Request[evalsiv1alpha1.ListAnnotationsRequest]) (*connect.Response[evalsiv1alpha1.ListAnnotationsResponse], error) {
	q, err := s.queue(ctx, req.Msg.GetProject(), req.Msg.GetQueue())
	if err != nil {
		return nil, err
	}
	size := int(req.Msg.GetPageSize())
	if size <= 0 || size > 1000 {
		size = 100
	}
	offset := 0
	if t := req.Msg.GetPageToken(); t != "" {
		if offset, err = strconv.Atoi(t); err != nil || offset < 0 {
			return nil, invalid("bad page_token")
		}
	}
	anns, err := s.store.QueueAnnotations(ctx, q.GetProject(), q.GetName(), offset, size+1)
	if err != nil {
		return nil, err
	}
	resp := &evalsiv1alpha1.ListAnnotationsResponse{}
	if len(anns) > size {
		anns = anns[:size]
		resp.NextPageToken = strconv.Itoa(offset + size)
	}
	resp.Annotations = anns
	seen := map[string]bool{}
	for _, a := range anns {
		if seen[a.GetItemId()] {
			continue
		}
		seen[a.GetItemId()] = true
		it, err := s.store.GetItem(ctx, q.GetProject(), q.GetName(), a.GetItemId())
		if err == nil {
			resp.Items = append(resp.Items, it)
		}
	}
	return connect.NewResponse(resp), nil
}

// SummarizeQueue summarizes a queue's answers.
func (s *Service) SummarizeQueue(ctx context.Context, req *connect.Request[evalsiv1alpha1.SummarizeQueueRequest]) (*connect.Response[evalsiv1alpha1.SummarizeQueueResponse], error) {
	q, err := s.queue(ctx, req.Msg.GetProject(), req.Msg.GetQueue())
	if err != nil {
		return nil, err
	}
	level := req.Msg.GetConfidenceLevel()
	if level == 0 {
		level = 0.95
	}
	if level <= 0 || level >= 1 {
		return nil, invalid("confidence_level must be between 0 and 1")
	}
	items, err := s.store.QueueItems(ctx, q.GetProject(), q.GetName(), 0, 0)
	if err != nil {
		return nil, err
	}
	anns, err := s.store.QueueAnnotations(ctx, q.GetProject(), q.GetName(), 0, 0)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(Summarize(q, items, anns, level)), nil
}

// Summarize computes a queue's statistics.
func Summarize(q *evalsiv1alpha1.AnnotationQueue, items []*evalsiv1alpha1.AnnotationItem, anns []*evalsiv1alpha1.Annotation, level float64) *evalsiv1alpha1.SummarizeQueueResponse {
	resp := &evalsiv1alpha1.SummarizeQueueResponse{Items: int64(len(items)), Annotators: map[string]int64{}}
	byItem := map[string][]*evalsiv1alpha1.Annotation{}
	for _, a := range anns {
		if a.GetSkipped() {
			resp.Skipped++
			continue
		}
		resp.Annotations++
		resp.Annotators[a.GetAnnotator()]++
		byItem[a.GetItemId()] = append(byItem[a.GetItemId()], a)
	}
	for _, it := range items {
		if it.GetDone() {
			resp.Done++
		}
	}
	for _, qu := range q.GetQuestions() {
		resp.Questions = append(resp.Questions, questionStats(qu, items, byItem, level))
	}
	return resp
}

func answerValue(a *evalsiv1alpha1.Annotation, question string) *evalsiv1alpha1.Answer {
	for _, ans := range a.GetAnswers() {
		if ans.GetQuestion() == question {
			return ans
		}
	}
	return nil
}

func questionStats(qu *evalsiv1alpha1.Question, items []*evalsiv1alpha1.AnnotationItem, byItem map[string][]*evalsiv1alpha1.Annotation, level float64) *evalsiv1alpha1.QuestionStats {
	out := &evalsiv1alpha1.QuestionStats{Question: qu.GetName()}
	kind := qu.GetKind()
	numeric := kind == evalsiv1alpha1.QuestionKind_QUESTION_KIND_PASS_FAIL || kind == evalsiv1alpha1.QuestionKind_QUESTION_KIND_SCORE
	var itemValues []float64
	var units [][]float64
	var labelUnits [][]string
	var human, machine []float64
	for _, it := range items {
		var vals []float64
		var labels []string
		for _, a := range byItem[it.GetId()] {
			ans := answerValue(a, qu.GetName())
			if ans == nil {
				continue
			}
			switch v := ans.GetValue().(type) {
			case *evalsiv1alpha1.Answer_Passed:
				if v.Passed {
					vals = append(vals, 1)
				} else {
					vals = append(vals, 0)
				}
			case *evalsiv1alpha1.Answer_Number:
				vals = append(vals, v.Number)
			case *evalsiv1alpha1.Answer_Label:
				labels = append(labels, v.Label)
				if out.LabelCounts == nil {
					out.LabelCounts = map[string]int64{}
				}
				out.LabelCounts[v.Label]++
			}
		}
		if numeric && len(vals) > 0 {
			m := stats.Mean(vals)
			itemValues = append(itemValues, m)
			if len(vals) > 1 {
				units = append(units, vals)
			}
			if metric := qu.GetCompareMetric(); metric != "" {
				if mv, ok := it.GetRunScores()[metric]; ok {
					human = append(human, m)
					machine = append(machine, mv)
				}
			}
		}
		if len(labels) > 1 {
			labelUnits = append(labelUnits, labels)
		}
	}
	if numeric {
		proportion := kind == evalsiv1alpha1.QuestionKind_QUESTION_KIND_PASS_FAIL
		sum := &evalsiv1alpha1.MetricSummary{Metric: qu.GetName(), N: int64(len(itemValues))}
		sum.Kind = evalsiv1alpha1.MetricKind_METRIC_KIND_NUMBER
		if proportion {
			sum.Kind = evalsiv1alpha1.MetricKind_METRIC_KIND_PROPORTION
		}
		if len(itemValues) > 0 {
			mean := stats.Mean(itemValues)
			sum.Mean = &mean
			if len(itemValues) > 1 {
				sd := stats.StdDev(itemValues)
				sum.Std = &sd
			}
			if iv, ok, _ := stats.ForMetric(itemValues, proportion, level, nil, ""); ok {
				sum.Ci = &evalsiv1alpha1.ConfidenceInterval{Low: iv.Low, High: iv.High, Level: iv.Level, Method: iv.Method}
				// A score's mean cannot leave the question's scale; a t
				// interval on few items can.
				if !proportion && qu.GetMax() > qu.GetMin() {
					sum.Ci.Low = max(sum.Ci.Low, qu.GetMin())
					sum.Ci.High = min(sum.Ci.High, qu.GetMax())
				}
			}
		}
		out.Summary = sum
		out.MultiplyAnnotated = int64(len(units))
		if proportion {
			if a, ok := alphaNominal(floatUnits(units)); ok {
				out.InterAnnotatorAlpha = &a
			}
		} else if a, ok := alphaInterval(units); ok {
			out.InterAnnotatorAlpha = &a
		}
		if len(human) > 0 {
			out.MetricAgreement = agreement(human, machine, proportion)
		}
	}
	if kind == evalsiv1alpha1.QuestionKind_QUESTION_KIND_LABEL {
		out.MultiplyAnnotated = int64(len(labelUnits))
		if a, ok := alphaNominal(labelUnits); ok {
			out.InterAnnotatorAlpha = &a
		}
	}
	return out
}

func floatUnits(units [][]float64) [][]string {
	out := make([][]string, len(units))
	for i, u := range units {
		for _, v := range u {
			out[i] = append(out[i], strconv.FormatFloat(v, 'g', -1, 64))
		}
	}
	return out
}
