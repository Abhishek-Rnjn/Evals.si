package guardrail

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/auth"
	"github.com/abhishek-rnjn/evals.si/internal/authz"
	"github.com/abhishek-rnjn/evals.si/internal/evaluation"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

// Service implements GuardrailService and the gateway protocols.
type Service struct {
	store *store.Store
	eval  *evaluation.Service
	log   *slog.Logger
	now   func() time.Time

	mu    sync.Mutex
	cache map[string]*compiled
	stats map[statKey]*stat
}

type statKey struct{ project, guardrail, phase, verdict string }

type stat struct {
	checks, blocked, failed int64
	seconds                 float64
}

// New makes the service.
func New(st *store.Store, eval *evaluation.Service, log *slog.Logger) *Service {
	return &Service{store: st, eval: eval, log: log, now: time.Now,
		cache: map[string]*compiled{}, stats: map[statKey]*stat{}}
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

// load returns a stored guardrail, compiled; recompiled when it changed.
func (s *Service) load(ctx context.Context, proj, name string) (*compiled, error) {
	g, err := s.store.GetGuardrail(ctx, proj, name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("guardrail %s/%s not found", proj, name))
	}
	if err != nil {
		return nil, err
	}
	key := proj + "/" + name
	version := g.GetUpdatedAt().AsTime().UnixNano()
	s.mu.Lock()
	c := s.cache[key]
	s.mu.Unlock()
	if c != nil && c.version == version {
		return c, nil
	}
	if c, err = compile(g, s.eval); err != nil {
		// Stored guardrails were valid when applied; a catalog change can
		// still break one (an evaluator uninstalled).
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("guardrail %s: %w", key, err))
	}
	s.mu.Lock()
	s.cache[key] = c
	s.mu.Unlock()
	return c, nil
}

// Run checks input against a stored guardrail and counts the outcome.
func (s *Service) Run(ctx context.Context, proj, name string, in Input) (*Result, error) {
	proj = project(proj)
	c, err := s.load(ctx, proj, name)
	if err != nil {
		return nil, err
	}
	return s.run(ctx, proj, c, in)
}

func (s *Service) run(ctx context.Context, proj string, c *compiled, in Input) (*Result, error) {
	size := 0
	for _, p := range in.Parts {
		size += len(p)
	}
	if size > MaxContent {
		return nil, invalid("content is %d bytes; at most %d", size, MaxContent)
	}
	res := c.check(ctx, s.eval, in)
	resp := res.Response
	verdict := strings.ToLower(strings.TrimPrefix(resp.GetVerdict().String(), "GUARDRAIL_DECISION_"))
	k := statKey{proj, c.g.GetName(), phaseName(in.Phase), verdict}
	s.mu.Lock()
	st := s.stats[k]
	if st == nil {
		st = &stat{}
		s.stats[k] = st
	}
	st.checks++
	if !res.Pass() {
		st.blocked++
	}
	if resp.GetFailed() {
		st.failed++
	}
	st.seconds += resp.GetDuration().AsDuration().Seconds()
	s.mu.Unlock()
	if resp.GetFailed() {
		s.log.Warn("guardrail evaluation failed", "project", proj, "guardrail", c.g.GetName(), "reason", resp.GetReason())
	}
	return res, nil
}

// ApplyGuardrail creates or replaces a guardrail.
func (s *Service) ApplyGuardrail(ctx context.Context, req *connect.Request[evalsiv1alpha1.ApplyGuardrailRequest]) (*connect.Response[evalsiv1alpha1.ApplyGuardrailResponse], error) {
	g := proto.Clone(req.Msg.GetGuardrail()).(*evalsiv1alpha1.Guardrail)
	if g == nil || g.GetName() == "" {
		return nil, invalid("guardrail.name is required")
	}
	g.Project = project(g.GetProject())
	g.UpdatedAt = timestamppb.New(s.now())
	if p := auth.PrincipalFrom(ctx); p != nil {
		g.UpdatedBy = p.ID()
	}
	if _, err := compile(g, s.eval); err != nil {
		return nil, invalid("%v", err)
	}
	if err := s.store.PutGuardrail(ctx, g); err != nil {
		return nil, err
	}
	return connect.NewResponse(&evalsiv1alpha1.ApplyGuardrailResponse{Guardrail: g}), nil
}

// ListGuardrails lists a project's guardrails.
func (s *Service) ListGuardrails(ctx context.Context, req *connect.Request[evalsiv1alpha1.ListGuardrailsRequest]) (*connect.Response[evalsiv1alpha1.ListGuardrailsResponse], error) {
	gs, err := s.store.ListGuardrails(ctx, project(req.Msg.GetProject()))
	if err != nil {
		return nil, err
	}
	// Each guardrail is checked by its own name and labels.
	visible := gs[:0]
	for _, g := range gs {
		if authz.Can(ctx, "guardrails.read", g.GetProject(), authz.GuardrailResource(g.GetName(), g.GetLabels())) {
			visible = append(visible, g)
		}
	}
	return connect.NewResponse(&evalsiv1alpha1.ListGuardrailsResponse{Guardrails: visible}), nil
}

// GetGuardrail returns a guardrail.
func (s *Service) GetGuardrail(ctx context.Context, req *connect.Request[evalsiv1alpha1.GetGuardrailRequest]) (*connect.Response[evalsiv1alpha1.GetGuardrailResponse], error) {
	proj := project(req.Msg.GetProject())
	g, err := s.store.GetGuardrail(ctx, proj, req.Msg.GetName())
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("guardrail %s/%s not found", proj, req.Msg.GetName()))
	}
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&evalsiv1alpha1.GetGuardrailResponse{Guardrail: g}), nil
}

// DeleteGuardrail removes a guardrail.
func (s *Service) DeleteGuardrail(ctx context.Context, req *connect.Request[evalsiv1alpha1.DeleteGuardrailRequest]) (*connect.Response[evalsiv1alpha1.DeleteGuardrailResponse], error) {
	proj := project(req.Msg.GetProject())
	err := s.store.DeleteGuardrail(ctx, proj, req.Msg.GetName())
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("guardrail %s/%s not found", proj, req.Msg.GetName()))
	}
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	delete(s.cache, proj+"/"+req.Msg.GetName())
	s.mu.Unlock()
	return connect.NewResponse(&evalsiv1alpha1.DeleteGuardrailResponse{}), nil
}

// Check checks content against a stored guardrail, or an inline one.
func (s *Service) Check(ctx context.Context, req *connect.Request[evalsiv1alpha1.CheckRequest]) (*connect.Response[evalsiv1alpha1.CheckResponse], error) {
	m := req.Msg
	phase := m.GetPhase()
	if phase == evalsiv1alpha1.GuardrailPhase_GUARDRAIL_PHASE_UNSPECIFIED {
		phase = evalsiv1alpha1.GuardrailPhase_GUARDRAIL_PHASE_REQUEST
	}
	in := Input{Phase: phase, Source: "api", Parts: []string{m.GetContent()}, Context: m.GetContext(), Labels: m.GetLabels()}
	var (
		res *Result
		err error
	)
	switch {
	case m.GetInline() != nil && m.GetGuardrail() != "":
		return nil, invalid("set guardrail or inline, not both")
	case m.GetInline() != nil:
		c, cerr := compile(m.GetInline(), s.eval)
		if cerr != nil {
			return nil, invalid("%v", cerr)
		}
		res, err = s.run(ctx, project(m.GetProject()), c, in)
	case m.GetGuardrail() != "":
		res, err = s.Run(ctx, m.GetProject(), m.GetGuardrail(), in)
	default:
		return nil, invalid("guardrail or inline is required")
	}
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(res.Response), nil
}

// WriteMetrics writes per-guardrail Prometheus counters.
func (s *Service) WriteMetrics(w io.Writer) {
	s.mu.Lock()
	keys := make([]statKey, 0, len(s.stats))
	for k := range s.stats {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		return a.project+"\x00"+a.guardrail+"\x00"+a.phase+"\x00"+a.verdict < b.project+"\x00"+b.guardrail+"\x00"+b.phase+"\x00"+b.verdict
	})
	rows := make([]stat, len(keys))
	for i, k := range keys {
		rows[i] = *s.stats[k]
	}
	s.mu.Unlock()
	series := []struct {
		name, help string
		value      func(stat) any
	}{
		{"evalsi_guardrail_checks_total", "Guardrail checks, by verdict (in audit mode, what would have happened).", func(st stat) any { return st.checks }},
		{"evalsi_guardrail_blocked_total", "Checks whose content was blocked.", func(st stat) any { return st.blocked }},
		{"evalsi_guardrail_failed_total", "Checks whose evaluators failed or timed out (failure_mode decided).", func(st stat) any { return st.failed }},
		{"evalsi_guardrail_check_seconds_total", "Time spent checking.", func(st stat) any { return st.seconds }},
	}
	for _, m := range series {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", m.name, m.help, m.name)
		for i, k := range keys {
			fmt.Fprintf(w, "%s{project=%q,guardrail=%q,phase=%q,verdict=%q} %v\n", m.name, k.project, k.guardrail, k.phase, k.verdict, m.value(rows[i]))
		}
	}
}
