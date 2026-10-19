// Package failures implements the human-reviewed production failure inbox.
package failures

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/auth"
	"github.com/abhishek-rnjn/evals.si/internal/authz"
	"github.com/abhishek-rnjn/evals.si/internal/datasets"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

const (
	MaxRecordBytes = 1 << 20
	MaxExportBytes = 2 << 20
	MaxSelections  = 100
)

var idRE = regexp.MustCompile(`^[0-9a-f]{24}$`)

type Service struct{ store *store.Store }

func New(st *store.Store) *Service { return &Service{store: st} }

func Project(p string) string {
	if p == "" {
		return authz.DefaultProject
	}
	return p
}

func actor(ctx context.Context) string {
	if p := auth.PrincipalFrom(ctx); p != nil {
		return p.ID()
	}
	return "anonymous"
}

func invalid(message string) error {
	return connect.NewError(connect.CodeInvalidArgument, errors.New(message))
}

func dbError(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return connect.NewError(connect.CodeNotFound, errors.New("failure or trace not found"))
	}
	if errors.Is(err, store.ErrFailureChanged) {
		return connect.NewError(connect.CodeAborted, err)
	}
	return err
}

// Readable preserves the source trace's project/service/label access rules,
// even if that trace expires from the ingest store later.
func Readable(ctx context.Context, f *pb.FailureCase) bool {
	resource := authz.TraceResource(f.GetSource())
	return authz.Can(ctx, "failures.read", f.GetProject(), resource) && authz.Can(ctx, "traces.read", f.GetProject(), resource)
}

func writable(ctx context.Context, action string, f *pb.FailureCase) error {
	if !authz.Can(ctx, action, f.GetProject(), authz.TraceResource(f.GetSource())) {
		return connect.NewError(connect.CodePermissionDenied, errors.New("failure action is not allowed"))
	}
	return nil
}

func (s *Service) get(ctx context.Context, project, id string) (*pb.FailureCase, error) {
	if !idRE.MatchString(id) {
		return nil, invalid("failure id must be 24 lowercase hex characters")
	}
	f, err := s.store.GetFailure(ctx, Project(project), id)
	if err != nil {
		return nil, dbError(err)
	}
	if !Readable(ctx, f) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("failure not found"))
	}
	return f, nil
}

func editable(title, reason, assignee string, record *pb.Record) error {
	if strings.TrimSpace(title) == "" || len(title) > 256 {
		return invalid("title must contain 1-256 bytes of text")
	}
	if strings.TrimSpace(reason) == "" || len(reason) > 4096 {
		return invalid("reason must contain 1-4096 bytes of text")
	}
	if len(assignee) > 256 || strings.ContainsAny(assignee, "\r\n\x00") {
		return invalid("assignee must be at most 256 bytes without line breaks")
	}
	if record == nil {
		return invalid("record is required")
	}
	if proto.Size(record) > MaxRecordBytes {
		return connect.NewError(connect.CodeResourceExhausted, errors.New("failure record exceeds 1 MiB"))
	}
	if _, exists := record.GetMetadata()["evalsi.dataset"]; exists {
		return invalid("record metadata uses reserved evalsi.dataset identity")
	}
	if _, exists := record.GetMetadata()["evalsi.failure"]; exists {
		return invalid("record metadata uses reserved evalsi.failure provenance")
	}
	return nil
}

func event(f *pb.FailureCase, action string) *pb.FailureEvent {
	return &pb.FailureEvent{Revision: f.GetRevision(), Actor: f.GetUpdatedBy(), Action: action, Status: f.GetStatus(), RecordSha256: store.RecordHash(f.GetRecord()), Time: f.GetUpdatedAt()}
}

func (s *Service) CaptureFailure(ctx context.Context, req *connect.Request[pb.CaptureFailureRequest]) (*connect.Response[pb.CaptureFailureResponse], error) {
	m := req.Msg
	p := Project(m.GetProject())
	summary, record, err := s.store.GetTrace(ctx, p, strings.ToLower(m.GetTraceId()))
	if err != nil {
		return nil, dbError(err)
	}
	f := &pb.FailureCase{Project: p, Source: summary, SourceSha256: store.RecordHash(record), Record: record, Title: m.GetTitle(), Reason: m.GetReason(), Status: pb.FailureStatus_FAILURE_STATUS_OPEN, Revision: 1, CreatedBy: actor(ctx), UpdatedBy: actor(ctx), CreatedAt: timestamppb.Now()}
	f.UpdatedAt = f.CreatedAt
	if !Readable(ctx, f) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("trace not found"))
	}
	// A late span can change the source summary after the gate loaded it.
	if err := writable(ctx, "failures.write", f); err != nil {
		return nil, err
	}
	if err := editable(f.GetTitle(), f.GetReason(), "", record); err != nil {
		return nil, err
	}
	saved, duplicate, err := s.store.CaptureFailure(ctx, f, event(f, "captured"))
	if err != nil {
		return nil, dbError(err)
	}
	// A matching record may have been captured under earlier source labels.
	// Recapture must not reveal that curated copy through a changed trace label.
	if !Readable(ctx, saved) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("failure not found"))
	}
	if err := writable(ctx, "failures.write", saved); err != nil {
		return nil, err
	}
	return connect.NewResponse(&pb.CaptureFailureResponse{Failure: saved, Deduplicated: duplicate}), nil
}

func pageSize(size int32) (int, error) {
	if size < 0 || size > 100 {
		return 0, invalid("page_size must be between 1 and 100")
	}
	if size == 0 {
		return 50, nil
	}
	return int(size), nil
}

func (s *Service) ListFailures(ctx context.Context, req *connect.Request[pb.ListFailuresRequest]) (*connect.Response[pb.ListFailuresResponse], error) {
	m := req.Msg
	size, err := pageSize(m.GetPageSize())
	if err != nil {
		return nil, err
	}
	if _, ok := pb.FailureStatus_name[int32(m.GetStatus())]; !ok {
		return nil, invalid("unknown failure status")
	}
	if m.GetPageToken() != "" && !idRE.MatchString(m.GetPageToken()) {
		return nil, invalid("invalid page token")
	}
	rows, next, err := s.store.ListFailures(ctx, Project(m.GetProject()), m.GetStatus(), m.GetAssignee(), size, m.GetPageToken())
	if err != nil {
		return nil, err
	}
	visible := rows[:0]
	for _, f := range rows {
		if Readable(ctx, f) {
			visible = append(visible, f)
		}
	}
	return connect.NewResponse(&pb.ListFailuresResponse{Failures: visible, NextPageToken: next}), nil
}

func (s *Service) GetFailure(ctx context.Context, req *connect.Request[pb.GetFailureRequest]) (*connect.Response[pb.GetFailureResponse], error) {
	f, err := s.get(ctx, req.Msg.GetProject(), req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&pb.GetFailureResponse{Failure: f}), nil
}

func checkRevision(f *pb.FailureCase, expected int64) error {
	if expected < 1 {
		return invalid("expected_revision is required and must be positive")
	}
	if f.GetRevision() != expected {
		return dbError(store.ErrFailureChanged)
	}
	return nil
}

func (s *Service) UpdateFailure(ctx context.Context, req *connect.Request[pb.UpdateFailureRequest]) (*connect.Response[pb.UpdateFailureResponse], error) {
	m := req.Msg
	f, err := s.get(ctx, m.GetProject(), m.GetId())
	if err != nil {
		return nil, err
	}
	if err := writable(ctx, "failures.write", f); err != nil {
		return nil, err
	}
	if err := checkRevision(f, m.GetExpectedRevision()); err != nil {
		return nil, err
	}
	if m.GetStatus() != pb.FailureStatus_FAILURE_STATUS_OPEN && m.GetStatus() != pb.FailureStatus_FAILURE_STATUS_IN_REVIEW {
		return nil, invalid("edits must set status OPEN or IN_REVIEW; approval requires review")
	}
	if err := editable(m.GetTitle(), m.GetReason(), m.GetAssignee(), m.GetRecord()); err != nil {
		return nil, err
	}
	f.Record = proto.Clone(m.GetRecord()).(*pb.Record)
	// Source identity is immutable even when a reviewer redacts or replaces fields.
	f.Record.Id = f.GetSource().GetTraceId()
	f.Record.Provenance = &pb.Provenance{Source: &pb.Provenance_Trace{Trace: &pb.TraceProvenance{TraceId: f.GetSource().GetTraceId()}}}
	f.Title, f.Reason, f.Assignee, f.Status = m.GetTitle(), m.GetReason(), m.GetAssignee(), m.GetStatus()
	f.ReviewedBy, f.ReviewNote, f.ReviewedAt = "", "", nil
	f.Revision++
	f.UpdatedBy, f.UpdatedAt = actor(ctx), timestamppb.Now()
	if err := s.store.SaveFailure(ctx, f, m.GetExpectedRevision(), event(f, "updated")); err != nil {
		return nil, dbError(err)
	}
	return connect.NewResponse(&pb.UpdateFailureResponse{Failure: f}), nil
}

func hasContent(content *pb.Content) bool {
	if content == nil || content.GetKind() == nil {
		return false
	}
	if text, ok := content.GetKind().(*pb.Content_Text); ok {
		return strings.TrimSpace(text.Text) != ""
	}
	if chat, ok := content.GetKind().(*pb.Content_Messages); ok {
		for _, message := range chat.Messages.GetMessages() {
			if strings.TrimSpace(message.GetContent()) != "" || len(message.GetToolCalls()) != 0 {
				return true
			}
		}
		return false
	}
	switch kind := content.GetKind().(type) {
	case *pb.Content_Json:
		return hasJSONContent(kind.Json)
	case *pb.Content_Row:
		return len(kind.Row.GetFeatures()) != 0 || len(kind.Row.GetProbabilities()) != 0 || hasJSONContent(kind.Row.GetPrediction())
	case *pb.Content_Tensor:
		return len(kind.Tensor.GetData()) != 0
	case *pb.Content_Media:
		return strings.TrimSpace(kind.Media.GetUri()) != ""
	}
	return false
}

func hasJSONContent(value *structpb.Value) bool {
	switch kind := value.GetKind().(type) {
	case nil, *structpb.Value_NullValue:
		return false
	case *structpb.Value_StringValue:
		return strings.TrimSpace(kind.StringValue) != ""
	case *structpb.Value_StructValue:
		return len(kind.StructValue.GetFields()) != 0
	case *structpb.Value_ListValue:
		return len(kind.ListValue.GetValues()) != 0
	default:
		return true
	}
}

func (s *Service) ReviewFailure(ctx context.Context, req *connect.Request[pb.ReviewFailureRequest]) (*connect.Response[pb.ReviewFailureResponse], error) {
	m := req.Msg
	f, err := s.get(ctx, m.GetProject(), m.GetId())
	if err != nil {
		return nil, err
	}
	if err := writable(ctx, "failures.review", f); err != nil {
		return nil, err
	}
	if err := checkRevision(f, m.GetExpectedRevision()); err != nil {
		return nil, err
	}
	if strings.TrimSpace(m.GetNote()) == "" || len(m.GetNote()) > 4096 {
		return nil, invalid("review note must contain 1-4096 bytes of text")
	}
	switch m.GetDecision() {
	case pb.FailureStatus_FAILURE_STATUS_APPROVED:
		if f.GetStatus() != pb.FailureStatus_FAILURE_STATUS_IN_REVIEW || !hasContent(f.GetRecord().GetInput()) || !hasContent(f.GetRecord().GetReference()) {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("approval requires IN_REVIEW with nonempty input and reviewed reference"))
		}
	case pb.FailureStatus_FAILURE_STATUS_DISMISSED:
	default:
		return nil, invalid("review decision must be APPROVED or DISMISSED")
	}
	f.Status, f.ReviewNote, f.ReviewedBy, f.ReviewedAt = m.GetDecision(), m.GetNote(), actor(ctx), timestamppb.Now()
	f.Revision++
	f.UpdatedBy, f.UpdatedAt = actor(ctx), f.ReviewedAt
	if err := s.store.SaveFailure(ctx, f, m.GetExpectedRevision(), event(f, "reviewed")); err != nil {
		return nil, dbError(err)
	}
	return connect.NewResponse(&pb.ReviewFailureResponse{Failure: f}), nil
}

func (s *Service) ListFailureEvents(ctx context.Context, req *connect.Request[pb.ListFailureEventsRequest]) (*connect.Response[pb.ListFailureEventsResponse], error) {
	m := req.Msg
	if _, err := s.get(ctx, m.GetProject(), m.GetId()); err != nil {
		return nil, err
	}
	size, err := pageSize(m.GetPageSize())
	if err != nil {
		return nil, err
	}
	var before int64
	if m.GetPageToken() != "" {
		before, err = strconv.ParseInt(m.GetPageToken(), 10, 64)
		if err != nil || before < 1 {
			return nil, invalid("invalid event page token")
		}
	}
	events, next, err := s.store.ListFailureEvents(ctx, Project(m.GetProject()), m.GetId(), size, before)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&pb.ListFailureEventsResponse{Events: events, NextPageToken: next}), nil
}

func (s *Service) ExportFailures(ctx context.Context, req *connect.Request[pb.ExportFailuresRequest]) (*connect.Response[pb.ExportFailuresResponse], error) {
	m := req.Msg
	if len(m.GetSelections()) < 1 || len(m.GetSelections()) > MaxSelections {
		return nil, invalid("export requires 1-100 explicit selections")
	}
	expected := map[string]int64{}
	var ids []string
	for _, selection := range m.GetSelections() {
		id := selection.GetId()
		if !idRE.MatchString(id) || selection.GetRevision() < 1 {
			return nil, invalid("each selection needs a valid id and positive revision")
		}
		if _, exists := expected[id]; exists {
			return nil, invalid("export selections must be unique")
		}
		expected[id] = selection.GetRevision()
		ids = append(ids, id)
	}
	cases, err := s.store.SelectedFailures(ctx, Project(m.GetProject()), ids)
	if err != nil {
		return nil, err
	}
	if len(cases) != len(ids) {
		return nil, dbError(store.ErrNotFound)
	}
	response := &pb.ExportFailuresResponse{}
	for _, f := range cases {
		if !Readable(ctx, f) {
			return nil, dbError(store.ErrNotFound)
		}
		if err := writable(ctx, "datasets.write", f); err != nil {
			return nil, err
		}
		if err := checkRevision(f, expected[f.GetId()]); err != nil {
			return nil, err
		}
		if f.GetStatus() != pb.FailureStatus_FAILURE_STATUS_APPROVED {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("every selected failure must be approved"))
		}
		record := proto.Clone(f.GetRecord()).(*pb.Record)
		record.Id = "failure/" + f.GetId()
		if record.Metadata == nil {
			record.Metadata = map[string]*structpb.Value{}
		}
		provenance, err := structpb.NewValue(map[string]any{"project": f.GetProject(), "case_id": f.GetId(), "revision": strconv.FormatInt(f.GetRevision(), 10), "trace_id": f.GetSource().GetTraceId(), "source_sha256": f.GetSourceSha256(), "reviewed_by": f.GetReviewedBy(), "reviewed_at": f.GetReviewedAt().AsTime().Format(time.RFC3339Nano), "record_sha256": store.RecordHash(f.GetRecord())})
		if err != nil {
			return nil, fmt.Errorf("failure provenance: %w", err)
		}
		record.Metadata["evalsi.failure"] = provenance
		response.Records = append(response.Records, record)
		line, err := datasets.Row(record, nil)
		if err != nil {
			return nil, err
		}
		response.Jsonl += string(line) + "\n"
		if proto.Size(response) > MaxExportBytes {
			return nil, connect.NewError(connect.CodeResourceExhausted, errors.New("export exceeds 2 MiB; select fewer cases"))
		}
	}
	return connect.NewResponse(response), nil
}

func (s *Service) DeleteFailure(ctx context.Context, req *connect.Request[pb.DeleteFailureRequest]) (*connect.Response[pb.DeleteFailureResponse], error) {
	m := req.Msg
	f, err := s.get(ctx, m.GetProject(), m.GetId())
	if err != nil {
		return nil, err
	}
	if err := writable(ctx, "failures.delete", f); err != nil {
		return nil, err
	}
	if err := checkRevision(f, m.GetExpectedRevision()); err != nil {
		return nil, err
	}
	if err := s.store.DeleteFailure(ctx, f.GetProject(), f.GetId(), m.GetExpectedRevision()); err != nil {
		return nil, dbError(err)
	}
	return connect.NewResponse(&pb.DeleteFailureResponse{}), nil
}
