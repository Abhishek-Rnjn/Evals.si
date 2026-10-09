package source

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/auth"
	"github.com/abhishek-rnjn/evals.si/internal/authz"
	"github.com/abhishek-rnjn/evals.si/internal/credentials"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// ServiceOptions are what the service checks a source against.
type ServiceOptions struct {
	Credentials *credentials.Policy
	// sources.allow_hosts.
	AllowHosts []string
	// Whether a policy exists in a project; nil skips the check.
	PolicyExists func(project, name string) bool
	Now          func() time.Time
}

// Service is the SourceService. Sources are stored here; the Manager (on the
// policy-engine leader) pulls them.
type Service struct {
	st   *store.Store
	mgr  *Manager
	opts ServiceOptions
}

// NewService returns the service over the manager's store.
func NewService(st *store.Store, mgr *Manager, opts ServiceOptions) *Service {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Service{st: st, mgr: mgr, opts: opts}
}

func invalid(format string, args ...any) error {
	return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(format, args...))
}

func project(p string) string {
	if p == "" {
		return authz.DefaultProject
	}
	return p
}

func notFound(proj, name string) error {
	return connect.NewError(connect.CodeNotFound, fmt.Errorf("source %s/%s not found", proj, name))
}

// Validate checks a source as ApplySource would, grants and endpoint included.
func (s *Service) Validate(ctx context.Context, src *evalsiv1alpha1.TraceSource) error {
	if !nameRE.MatchString(src.GetName()) {
		return invalid("source.name must be lowercase letters, digits, '.', '_' or '-' (at most 63 characters)")
	}
	factory := s.mgr.opts.Factories[src.GetConnector()]
	switch src.GetConnector() {
	case "":
		return invalid("source.connector is required (\"mlflow\")")
	case "langfuse", "phoenix":
		if factory == nil {
			return invalid("source.connector %q is designed (decision 0016) but not built yet", src.GetConnector())
		}
	}
	if factory == nil {
		return invalid("source.connector %q is unknown", src.GetConnector())
	}
	if err := CheckEndpoint(src.GetEndpoint(), s.opts.AllowHosts); err != nil {
		return invalid("%v", err)
	}
	// The connector knows its own variants and locations.
	if _, err := factory(src, "", http.DefaultClient); err != nil {
		return invalid("%v", err)
	}
	if use, ok, err := credentials.SourceUse(src); err != nil {
		return invalid("%v", err)
	} else if ok {
		if err := s.opts.Credentials.Check(ctx, project(src.GetProject()), []credentials.Use{use}); err != nil {
			return err
		}
	}
	if len(src.GetOverrides()) > 0 {
		return invalid("source.overrides is not built yet (decision 0016, item 20)")
	}
	if p := src.GetProfile(); p != "" && p != "auto" && p != src.GetConnector() {
		return invalid("source.profile %q is unknown (\"auto\", or %q)", p, src.GetConnector())
	}
	if d := src.GetPoll().GetInterval(); d != nil && d.AsDuration() < time.Second {
		return invalid("source.poll.interval must be at least 1s")
	}
	if src.GetPoll().GetMaxRecordsPerSecond() < 0 {
		return invalid("source.poll.max_records_per_second must not be negative")
	}
	if d := src.GetMaxTraceDuration(); d != nil && (d.AsDuration() < time.Minute || d.AsDuration() > 7*24*time.Hour) {
		return invalid("source.max_trace_duration must be between 1m and 168h")
	}
	if d := src.GetMaxInProgressAge(); d != nil && d.AsDuration() < time.Minute {
		return invalid("source.max_in_progress_age must be at least 1m")
	}
	if d := src.GetBackfill().GetSince(); d != nil && d.AsDuration() < 0 {
		return invalid("source.backfill.since must not be negative")
	}
	if s.opts.PolicyExists != nil {
		for _, name := range src.GetPolicies() {
			if !s.opts.PolicyExists(project(src.GetProject()), name) {
				return invalid("source.policies: policy %q does not exist in project %q", name, project(src.GetProject()))
			}
		}
	}
	return nil
}

// ApplySource creates or replaces a source.
func (s *Service) ApplySource(ctx context.Context, req *connect.Request[evalsiv1alpha1.ApplySourceRequest]) (*connect.Response[evalsiv1alpha1.ApplySourceResponse], error) {
	src, _ := proto.Clone(req.Msg.GetSource()).(*evalsiv1alpha1.TraceSource)
	if src == nil {
		return nil, invalid("source is required")
	}
	src.Project = project(src.GetProject())
	src.Status = nil
	if err := s.Validate(ctx, src); err != nil {
		return nil, err
	}
	if !req.Msg.GetValidateOnly() {
		src.UpdatedAt = timestamppb.New(s.opts.Now())
		if p := auth.PrincipalFrom(ctx); p != nil {
			src.UpdatedBy = p.ID()
		}
		if err := s.st.PutSource(ctx, src); err != nil {
			return nil, err
		}
	}
	out, err := s.mgr.Status(ctx, src)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&evalsiv1alpha1.ApplySourceResponse{Source: out}), nil
}

func (s *Service) get(ctx context.Context, proj, name string) (*evalsiv1alpha1.TraceSource, error) {
	src, err := s.st.GetSource(ctx, project(proj), name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFound(project(proj), name)
	}
	return src, err
}

// ListSources lists a project's sources the caller may read.
func (s *Service) ListSources(ctx context.Context, req *connect.Request[evalsiv1alpha1.ListSourcesRequest]) (*connect.Response[evalsiv1alpha1.ListSourcesResponse], error) {
	srcs, err := s.st.ListSources(ctx, project(req.Msg.GetProject()))
	if err != nil {
		return nil, err
	}
	out := &evalsiv1alpha1.ListSourcesResponse{}
	for _, src := range srcs {
		if !authz.Can(ctx, "sources.read", src.GetProject(), authz.SourceResource(src.GetName(), src.GetLabels())) {
			continue
		}
		withStatus, err := s.mgr.Status(ctx, src)
		if err != nil {
			return nil, err
		}
		out.Sources = append(out.Sources, withStatus)
	}
	return connect.NewResponse(out), nil
}

// GetSource returns a source with its status.
func (s *Service) GetSource(ctx context.Context, req *connect.Request[evalsiv1alpha1.GetSourceRequest]) (*connect.Response[evalsiv1alpha1.GetSourceResponse], error) {
	src, err := s.get(ctx, req.Msg.GetProject(), req.Msg.GetName())
	if err != nil {
		return nil, err
	}
	out, err := s.mgr.Status(ctx, src)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&evalsiv1alpha1.GetSourceResponse{Source: out}), nil
}

// DeleteSource removes a source and what the manager kept for it. Traces
// already pulled stay.
func (s *Service) DeleteSource(ctx context.Context, req *connect.Request[evalsiv1alpha1.DeleteSourceRequest]) (*connect.Response[evalsiv1alpha1.DeleteSourceResponse], error) {
	proj := project(req.Msg.GetProject())
	if err := s.st.DeleteSource(ctx, proj, req.Msg.GetName()); errors.Is(err, store.ErrNotFound) {
		return nil, notFound(proj, req.Msg.GetName())
	} else if err != nil {
		return nil, err
	}
	return connect.NewResponse(&evalsiv1alpha1.DeleteSourceResponse{}), nil
}

func (s *Service) setPaused(ctx context.Context, proj, name string, paused bool) (*evalsiv1alpha1.TraceSource, error) {
	src, err := s.get(ctx, proj, name)
	if err != nil {
		return nil, err
	}
	src.Paused = paused
	src.UpdatedAt = timestamppb.New(s.opts.Now())
	if p := auth.PrincipalFrom(ctx); p != nil {
		src.UpdatedBy = p.ID()
	}
	if err := s.st.PutSource(ctx, src); err != nil {
		return nil, err
	}
	return s.mgr.Status(ctx, src)
}

// PauseSource stops pulling and writing back; the watermark is kept.
func (s *Service) PauseSource(ctx context.Context, req *connect.Request[evalsiv1alpha1.PauseSourceRequest]) (*connect.Response[evalsiv1alpha1.PauseSourceResponse], error) {
	out, err := s.setPaused(ctx, req.Msg.GetProject(), req.Msg.GetName(), true)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&evalsiv1alpha1.PauseSourceResponse{Source: out}), nil
}

// ResumeSource starts pulling again from the stored watermark.
func (s *Service) ResumeSource(ctx context.Context, req *connect.Request[evalsiv1alpha1.ResumeSourceRequest]) (*connect.Response[evalsiv1alpha1.ResumeSourceResponse], error) {
	out, err := s.setPaused(ctx, req.Msg.GetProject(), req.Msg.GetName(), false)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&evalsiv1alpha1.ResumeSourceResponse{Source: out}), nil
}

// BackfillSource moves the watermark back so history is read again, ahead of
// tailing. Traces already scored and unchanged are not scored again.
func (s *Service) BackfillSource(ctx context.Context, req *connect.Request[evalsiv1alpha1.BackfillSourceRequest]) (*connect.Response[evalsiv1alpha1.BackfillSourceResponse], error) {
	src, err := s.get(ctx, req.Msg.GetProject(), req.Msg.GetName())
	if err != nil {
		return nil, err
	}
	var since time.Time
	switch {
	case req.Msg.GetSince().IsValid():
		since = req.Msg.GetSince().AsTime()
	case src.GetBackfill().GetSince().AsDuration() > 0:
		since = s.opts.Now().Add(-src.GetBackfill().GetSince().AsDuration())
	default:
		return nil, invalid("pass since, or set backfill.since on the source")
	}
	if since.After(s.opts.Now()) {
		return nil, invalid("since is in the future")
	}
	st, err := s.st.SourceState(ctx, src.GetProject(), src.GetName())
	if err != nil {
		return nil, err
	}
	st.Watermark, st.BackfillFrom = since.UTC(), since.UTC()
	if err := s.st.PutSourceState(ctx, src.GetProject(), src.GetName(), st); err != nil {
		return nil, err
	}
	out, err := s.mgr.Status(ctx, src)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&evalsiv1alpha1.BackfillSourceResponse{Source: out}), nil
}
