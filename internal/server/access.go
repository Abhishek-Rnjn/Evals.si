package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/structpb"

	extmcp "github.com/abhishek-rnjn/evals.si/gen/go/agentgateway/dev/ext_mcp"
	"github.com/abhishek-rnjn/evals.si/gen/go/agentgateway/dev/ext_mcp/ext_mcpconnect"
	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1/evalsiv1alpha1connect"
	"github.com/abhishek-rnjn/evals.si/internal/auth"
	"github.com/abhishek-rnjn/evals.si/internal/authz"
	"github.com/abhishek-rnjn/evals.si/internal/datasets"
	"github.com/abhishek-rnjn/evals.si/internal/guardrail"
	"github.com/abhishek-rnjn/evals.si/internal/ingest"
	"github.com/abhishek-rnjn/evals.si/internal/rewards"
	"github.com/abhishek-rnjn/evals.si/internal/store"
	"github.com/abhishek-rnjn/evals.si/internal/watch"
)

// The gate is the single enforcement point: every RPC passes its Connect
// interceptor, and every plain HTTP route other than /healthz, the login
// discovery document and the web UI's static files passes guardHTTP or a
// check of its own (authorizeTool for /mcp, authorizeGuardrail for the
// guardrail webhook). Each RPC has an entry in the action
// table (accessRules); a test walks the service descriptors so that no RPC
// can ship without one.

// target is one resource a call touches.
type target struct {
	project  string
	resource map[string]any
	// For the audit log, for example "run/3f2a".
	name string
	// A different action than the rule's, for a resource the call only
	// reads (the traces or the earlier run a new run's dataset comes from).
	action string
}

// accessRule says how one RPC is authorized.
type accessRule struct {
	action string
	// resolve lists what the call touches; each target must be allowed.
	resolve func(ctx context.Context, msg any) ([]target, error)
	// The service filters its results (or assigns projects) per item itself;
	// the gate only authenticates.
	filtered bool
	// Allowed when the action is allowed in any project (catalog.read).
	anyProject bool
	// The service writes its own, more detailed, audit events.
	serviceAudits bool
}

type gate struct {
	engine   *authz.Engine
	auditor  *authz.Auditor
	store    *store.Store
	watcher  *watch.Engine
	authSvc  *authz.Service
	runsCode authz.RunsCode
	log      *slog.Logger
	rules    map[string]accessRule
}

func newGate(engine *authz.Engine, auditor *authz.Auditor, st *store.Store, watcher *watch.Engine, authSvc *authz.Service, runsCode authz.RunsCode, log *slog.Logger) *gate {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	g := &gate{engine: engine, auditor: auditor, store: st, watcher: watcher, authSvc: authSvc, runsCode: runsCode, log: log}
	g.rules = g.accessRules()
	return g
}

// Reflection procedures, authorized as catalog.read.
const (
	reflectV1      = "/grpc.reflection.v1.ServerReflection/ServerReflectionInfo"
	reflectV1Alpha = "/grpc.reflection.v1alpha.ServerReflection/ServerReflectionInfo"
)

func (g *gate) accessRules() map[string]accessRule {
	catalog := accessRule{action: "catalog.read", anyProject: true}
	return map[string]accessRule{
		evalsiv1alpha1connect.EvaluationServiceEvaluateProcedure: {action: "evaluations.run", resolve: func(ctx context.Context, msg any) ([]target, error) {
			m := msg.(*evalsiv1alpha1.EvaluateRequest)
			return g.evaluateTarget(m.GetProject(), m.GetEvaluators(), m.GetJudge(), len(m.GetRecords()))
		}},
		evalsiv1alpha1connect.EvaluationServiceEvaluateStreamProcedure: {action: "evaluations.run", resolve: func(ctx context.Context, msg any) ([]target, error) {
			c := msg.(*evalsiv1alpha1.EvaluateStreamRequest).GetConfig()
			return g.evaluateTarget(c.GetProject(), c.GetEvaluators(), c.GetJudge(), 0)
		}},
		evalsiv1alpha1connect.RewardServiceScoreRewardsProcedure: {action: "evaluations.run", resolve: func(ctx context.Context, msg any) ([]target, error) {
			m := msg.(*evalsiv1alpha1.ScoreRewardsRequest)
			return g.evaluateTarget(m.GetProject(), rewards.Refs(m.GetSpec()), m.GetSpec().GetJudge(), len(m.GetRollouts()))
		}},
		evalsiv1alpha1connect.CatalogServiceListEvaluatorsProcedure: catalog,
		reflectV1:      catalog,
		reflectV1Alpha: catalog,

		evalsiv1alpha1connect.RunServiceCreateRunProcedure: {action: "runs.create", resolve: func(ctx context.Context, msg any) ([]target, error) {
			m := msg.(*evalsiv1alpha1.CreateRunRequest)
			return g.newRunTargets(ctx, m.GetProject(), m.GetSpec(), m.GetLabels(), "run/new")
		}},
		evalsiv1alpha1connect.RunServiceCreateShadowReplayProcedure: {action: "runs.create", resolve: func(ctx context.Context, msg any) ([]target, error) {
			m := msg.(*evalsiv1alpha1.CreateShadowReplayRequest)
			return g.newRunTargets(ctx, m.GetProject(), m.GetCandidate(), m.GetLabels(), "shadow/new")
		}},
		evalsiv1alpha1connect.RunServicePromoteResultsProcedure: {action: "datasets.write", resolve: func(ctx context.Context, msg any) ([]target, error) {
			m := msg.(*evalsiv1alpha1.PromoteResultsRequest)
			ts, err := g.runTargets(ctx, m.GetRunId())
			if err != nil {
				return nil, err
			}
			for _, t := range ts {
				ts = append(ts, target{project: t.project, resource: t.resource, name: t.name, action: "runs.read"})
				if t.resource != nil {
					t.resource["dataset"] = map[string]any{"name": m.GetDataset()}
				}
			}
			return ts, nil
		}},
		evalsiv1alpha1connect.RunServiceGetRunProcedure: {action: "runs.read", resolve: func(ctx context.Context, msg any) ([]target, error) {
			return g.runTargets(ctx, msg.(*evalsiv1alpha1.GetRunRequest).GetId())
		}},
		evalsiv1alpha1connect.RunServiceWatchRunProcedure: {action: "runs.read", resolve: func(ctx context.Context, msg any) ([]target, error) {
			return g.runTargets(ctx, msg.(*evalsiv1alpha1.WatchRunRequest).GetId())
		}},
		evalsiv1alpha1connect.RunServiceListRunResultsProcedure: {action: "runs.read", resolve: func(ctx context.Context, msg any) ([]target, error) {
			return g.runTargets(ctx, msg.(*evalsiv1alpha1.ListRunResultsRequest).GetRunId())
		}},
		evalsiv1alpha1connect.RunServiceCompareRunsProcedure: {action: "runs.read", resolve: func(ctx context.Context, msg any) ([]target, error) {
			m := msg.(*evalsiv1alpha1.CompareRunsRequest)
			return g.runTargets(ctx, m.GetBaselineRunId(), m.GetCandidateRunId())
		}},
		evalsiv1alpha1connect.RunServiceListRunsProcedure: {action: "runs.read", filtered: true},
		evalsiv1alpha1connect.RunServiceCancelRunProcedure: {action: "runs.cancel", resolve: func(ctx context.Context, msg any) ([]target, error) {
			return g.runTargets(ctx, msg.(*evalsiv1alpha1.CancelRunRequest).GetId())
		}},
		evalsiv1alpha1connect.RunServiceResumeRunProcedure: {action: "runs.resume", resolve: func(ctx context.Context, msg any) ([]target, error) {
			return g.runTargets(ctx, msg.(*evalsiv1alpha1.ResumeRunRequest).GetId())
		}},

		evalsiv1alpha1connect.MonitorServiceApplyPolicyProcedure: {action: "policies.write", resolve: g.applyPolicyTargets},
		evalsiv1alpha1connect.MonitorServiceDeletePolicyProcedure: {action: "policies.write", resolve: func(ctx context.Context, msg any) ([]target, error) {
			return g.policyTarget(msg.(*evalsiv1alpha1.DeletePolicyRequest).GetName()), nil
		}},
		evalsiv1alpha1connect.MonitorServiceGetPolicyStatsProcedure: {action: "policies.read", resolve: func(ctx context.Context, msg any) ([]target, error) {
			return g.policyTarget(msg.(*evalsiv1alpha1.GetPolicyStatsRequest).GetName()), nil
		}},
		evalsiv1alpha1connect.MonitorServiceListPoliciesProcedure: {action: "policies.read", filtered: true},

		evalsiv1alpha1connect.TraceServiceListTracesProcedure: {action: "traces.read", filtered: true},
		evalsiv1alpha1connect.TraceServiceGetTraceProcedure:   {action: "traces.read", filtered: true},
		ingest.TraceExportProcedure:                           {action: "traces.write", filtered: true},

		evalsiv1alpha1connect.AuthServiceWhoAmIProcedure:          {action: "self.read", filtered: true},
		evalsiv1alpha1connect.AuthServiceListProjectsProcedure:    {action: "self.read", filtered: true},
		evalsiv1alpha1connect.AuthServiceListRolesProcedure:       {action: "self.read", filtered: true},
		evalsiv1alpha1connect.AuthServiceListPermissionsProcedure: {action: "self.read", filtered: true},
		evalsiv1alpha1connect.AuthServiceCreateProjectProcedure: {action: "projects.manage", serviceAudits: true, resolve: func(ctx context.Context, msg any) ([]target, error) {
			return []target{{name: "project/" + msg.(*evalsiv1alpha1.CreateProjectRequest).GetName()}}, nil
		}},
		evalsiv1alpha1connect.AuthServiceCreateAPIKeyProcedure: {action: "access.manage", serviceAudits: true, resolve: func(ctx context.Context, msg any) ([]target, error) {
			m := msg.(*evalsiv1alpha1.CreateAPIKeyRequest)
			return g.keyTargets(m.GetName(), rolesOf(m.GetRoles()))
		}},
		evalsiv1alpha1connect.AuthServiceRevokeAPIKeyProcedure: {action: "access.manage", serviceAudits: true, resolve: func(ctx context.Context, msg any) ([]target, error) {
			name := msg.(*evalsiv1alpha1.RevokeAPIKeyRequest).GetName()
			roles, _, err := g.authSvc.KeyRoles(ctx, name)
			if err != nil {
				return []target{{name: "apikey/" + name}}, nil // unknown: owners learn it is missing
			}
			return g.keyTargets(name, roles)
		}},
		evalsiv1alpha1connect.AuthServiceListAPIKeysProcedure: {action: "access.manage", filtered: true},
		evalsiv1alpha1connect.AuthServiceCreateRoleProcedure: {action: "access.manage", serviceAudits: true, resolve: func(ctx context.Context, msg any) ([]target, error) {
			return g.roleTarget(msg.(*evalsiv1alpha1.CreateRoleRequest).GetRole().GetProject(), msg.(*evalsiv1alpha1.CreateRoleRequest).GetRole())
		}},
		evalsiv1alpha1connect.AuthServiceUpdateRoleProcedure: {action: "access.manage", serviceAudits: true, resolve: func(ctx context.Context, msg any) ([]target, error) {
			return g.roleTarget(msg.(*evalsiv1alpha1.UpdateRoleRequest).GetRole().GetProject(), msg.(*evalsiv1alpha1.UpdateRoleRequest).GetRole())
		}},
		evalsiv1alpha1connect.AuthServiceDeleteRoleProcedure: {action: "access.manage", serviceAudits: true, resolve: func(ctx context.Context, msg any) ([]target, error) {
			m := msg.(*evalsiv1alpha1.DeleteRoleRequest)
			return g.roleTarget(m.GetProject(), &evalsiv1alpha1.Role{Name: m.GetName(), Project: m.GetProject()})
		}},
		evalsiv1alpha1connect.AuthServiceListBindingsProcedure: {action: "access.manage", filtered: true},
		evalsiv1alpha1connect.AuthServiceCreateBindingProcedure: {action: "access.manage", serviceAudits: true, resolve: func(ctx context.Context, msg any) ([]target, error) {
			return g.bindingTarget(msg.(*evalsiv1alpha1.CreateBindingRequest).GetBinding())
		}},
		evalsiv1alpha1connect.AuthServiceDeleteBindingProcedure: {action: "access.manage", serviceAudits: true, resolve: func(ctx context.Context, msg any) ([]target, error) {
			return g.bindingTarget(msg.(*evalsiv1alpha1.DeleteBindingRequest).GetBinding())
		}},
		evalsiv1alpha1connect.AuthServiceListAuditEventsProcedure: {action: "audit.read", filtered: true},

		evalsiv1alpha1connect.AnnotationServiceCreateQueueProcedure: {action: "annotations.manage", resolve: func(ctx context.Context, msg any) ([]target, error) {
			q := msg.(*evalsiv1alpha1.CreateQueueRequest).GetQueue()
			return g.queueTarget(q.GetProject(), q.GetName(), q.GetLabels())
		}},
		evalsiv1alpha1connect.AnnotationServiceDeleteQueueProcedure: {action: "annotations.manage", resolve: func(ctx context.Context, msg any) ([]target, error) {
			m := msg.(*evalsiv1alpha1.DeleteQueueRequest)
			return g.queueTarget(m.GetProject(), m.GetName(), nil)
		}},
		evalsiv1alpha1connect.AnnotationServiceAddItemsProcedure: {action: "annotations.manage", resolve: func(ctx context.Context, msg any) ([]target, error) {
			m := msg.(*evalsiv1alpha1.AddItemsRequest)
			ts, err := g.queueTarget(m.GetProject(), m.GetQueue(), nil)
			if err != nil || m.GetRun() == nil {
				return ts, err
			}
			// Copying a run's records into a queue reads the run.
			runs, err := g.runTargets(ctx, m.GetRun().GetRunId())
			if err != nil {
				return nil, err
			}
			for _, t := range runs {
				t.action = "runs.read"
				ts = append(ts, t)
			}
			return ts, nil
		}},
		evalsiv1alpha1connect.AnnotationServiceListQueuesProcedure: {action: "annotations.read", resolve: func(ctx context.Context, msg any) ([]target, error) {
			return g.queueTarget(msg.(*evalsiv1alpha1.ListQueuesRequest).GetProject(), "", nil)
		}},
		evalsiv1alpha1connect.AnnotationServiceGetQueueProcedure: {action: "annotations.read", resolve: func(ctx context.Context, msg any) ([]target, error) {
			m := msg.(*evalsiv1alpha1.GetQueueRequest)
			return g.queueTarget(m.GetProject(), m.GetName(), nil)
		}},
		evalsiv1alpha1connect.AnnotationServiceListAnnotationsProcedure: {action: "annotations.read", resolve: func(ctx context.Context, msg any) ([]target, error) {
			m := msg.(*evalsiv1alpha1.ListAnnotationsRequest)
			return g.queueTarget(m.GetProject(), m.GetQueue(), nil)
		}},
		evalsiv1alpha1connect.AnnotationServiceSummarizeQueueProcedure: {action: "annotations.read", resolve: func(ctx context.Context, msg any) ([]target, error) {
			m := msg.(*evalsiv1alpha1.SummarizeQueueRequest)
			return g.queueTarget(m.GetProject(), m.GetQueue(), nil)
		}},
		evalsiv1alpha1connect.AnnotationServiceNextItemProcedure: {action: "annotations.write", resolve: func(ctx context.Context, msg any) ([]target, error) {
			m := msg.(*evalsiv1alpha1.NextItemRequest)
			return g.queueTarget(m.GetProject(), m.GetQueue(), nil)
		}},
		evalsiv1alpha1connect.AnnotationServiceSubmitAnnotationProcedure: {action: "annotations.write", resolve: func(ctx context.Context, msg any) ([]target, error) {
			m := msg.(*evalsiv1alpha1.SubmitAnnotationRequest)
			return g.queueTarget(m.GetProject(), m.GetQueue(), nil)
		}},

		evalsiv1alpha1connect.GuardrailServiceApplyGuardrailProcedure: {action: "guardrails.write", resolve: func(ctx context.Context, msg any) ([]target, error) {
			gr := msg.(*evalsiv1alpha1.ApplyGuardrailRequest).GetGuardrail()
			return g.guardrailTarget(gr.GetProject(), gr.GetName(), gr.GetLabels())
		}},
		evalsiv1alpha1connect.GuardrailServiceDeleteGuardrailProcedure: {action: "guardrails.write", resolve: func(ctx context.Context, msg any) ([]target, error) {
			m := msg.(*evalsiv1alpha1.DeleteGuardrailRequest)
			return g.guardrailTarget(m.GetProject(), m.GetName(), nil)
		}},
		evalsiv1alpha1connect.GuardrailServiceListGuardrailsProcedure: {action: "guardrails.read", resolve: func(ctx context.Context, msg any) ([]target, error) {
			return g.guardrailTarget(msg.(*evalsiv1alpha1.ListGuardrailsRequest).GetProject(), "", nil)
		}},
		evalsiv1alpha1connect.GuardrailServiceGetGuardrailProcedure: {action: "guardrails.read", resolve: func(ctx context.Context, msg any) ([]target, error) {
			m := msg.(*evalsiv1alpha1.GetGuardrailRequest)
			return g.guardrailTarget(m.GetProject(), m.GetName(), nil)
		}},
		evalsiv1alpha1connect.GuardrailServiceCheckProcedure: {action: "guardrails.check", resolve: func(ctx context.Context, msg any) ([]target, error) {
			m := msg.(*evalsiv1alpha1.CheckRequest)
			if m.GetInline() == nil {
				return g.guardrailTarget(m.GetProject(), m.GetGuardrail(), nil)
			}
			// An inline guardrail is a dry run: it scores like Evaluate does.
			ts, err := g.evaluateTarget(m.GetProject(), m.GetInline().GetEvaluators(), m.GetInline().GetJudge(), 1)
			for i := range ts {
				ts[i].action = "evaluations.run"
			}
			return ts, err
		}},
		ext_mcpconnect.ExtMcpCheckRequestProcedure: {action: "guardrails.check", resolve: func(ctx context.Context, msg any) ([]target, error) {
			return g.mcpGuardrailTarget(msg.(*extmcp.McpRequest).GetMetadataContext())
		}},
		ext_mcpconnect.ExtMcpCheckResponseProcedure: {action: "guardrails.check", resolve: func(ctx context.Context, msg any) ([]target, error) {
			return g.mcpGuardrailTarget(msg.(*extmcp.McpResponse).GetMetadataContext())
		}},
	}
}

// guardrailTarget is a guardrail: its project, with its name and labels for
// rules (resource.guardrail, resource.labels).
func (g *gate) guardrailTarget(project, name string, labels map[string]string) ([]target, error) {
	project, err := g.project(project)
	if err != nil {
		return nil, err
	}
	res := map[string]any{"guardrail": map[string]any{"name": name}, "labels": authz.StringMap(labels)}
	return []target{{project: project, resource: res, name: "guardrail/" + name}}, nil
}

func (g *gate) mcpGuardrailTarget(md *structpb.Struct) ([]target, error) {
	project, name, err := guardrail.MCPTarget(md)
	if err != nil {
		return nil, err
	}
	return g.guardrailTarget(project, name, nil)
}

// authorizeGuardrail decides whether a guardrail webhook call may check
// content against the guardrail (guardrails.check; not audited: a gateway
// calls it for every request).
func (g *gate) authorizeGuardrail(r *http.Request, project, name string) error {
	ctx, chk, err := g.authenticate(r.Context(), "POST /guardrails", "http", r.RemoteAddr, r.Header)
	if err != nil {
		return err
	}
	if !g.engine.Enabled() {
		return nil
	}
	ts, err := g.guardrailTarget(project, name, nil)
	if err != nil {
		return err
	}
	if d := g.decide(ctx, chk, "guardrails.check", ts[0], false); !d.Allowed {
		return permissionDenied("guardrails.check", ts[0])
	}
	return nil
}

// queueTarget is an annotation queue: its project, with the queue's name
// and labels for rules (resource.queue, resource.labels).
func (g *gate) queueTarget(project, name string, labels map[string]string) ([]target, error) {
	project, err := g.project(project)
	if err != nil {
		return nil, err
	}
	res := map[string]any{"queue": map[string]any{"name": name}, "labels": authz.StringMap(labels)}
	return []target{{project: project, resource: res, name: "queue/" + name}}, nil
}

// project normalizes a request's project: empty means the default project,
// and with access control on, the project must exist.
func (g *gate) project(name string) (string, error) {
	if name == "" {
		return authz.DefaultProject, nil
	}
	if g.engine.Enabled() && !g.engine.ProjectExists(name) {
		return "", connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unknown project %q", name))
	}
	return name, nil
}

func (g *gate) evaluateTarget(project string, refs []*evalsiv1alpha1.EvaluatorRef, judge string, records int) ([]target, error) {
	project, err := g.project(project)
	if err != nil {
		return nil, err
	}
	return []target{{project: project, resource: authz.EvaluateResource(refs, judge, records, g.runsCode), name: "evaluation"}}, nil
}

// newRunTargets is a new run in a project plus what its dataset reads.
func (g *gate) newRunTargets(ctx context.Context, project string, spec *evalsiv1alpha1.RunSpec, labels map[string]string, name string) ([]target, error) {
	project, err := g.project(project)
	if err != nil {
		return nil, err
	}
	extra, err := g.datasetTargets(ctx, project, spec.GetDataset())
	if err != nil {
		return nil, err
	}
	return append([]target{{project: project, resource: authz.SpecResource(spec, labels, g.runsCode), name: name}}, extra...), nil
}

// runTargets loads runs. An unknown run becomes a target in no project,
// which only owners pass (and then get NotFound), so a missing run and a
// forbidden one look the same to everyone else.
func (g *gate) runTargets(ctx context.Context, ids ...string) ([]target, error) {
	var out []target
	for _, id := range ids {
		run, err := g.store.GetRun(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			out = append(out, target{name: "run/" + id})
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, target{project: run.GetProject(), resource: authz.RunResource(run, g.runsCode), name: "run/" + id})
	}
	return out, nil
}

func (g *gate) policyTarget(name string) []target {
	p, ok := g.watcher.Policy(name)
	if !ok {
		return []target{{name: "policy/" + name}}
	}
	return []target{{project: p.GetProject(), resource: authz.PolicyResource(p, g.runsCode), name: "policy/" + name}}
}

// applyPolicyTargets checks the new policy and, when it replaces one in
// another project, that project too: policy names are install-wide.
func (g *gate) applyPolicyTargets(_ context.Context, msg any) ([]target, error) {
	p := msg.(*evalsiv1alpha1.ApplyPolicyRequest).GetPolicy()
	project, err := g.project(p.GetProject())
	if err != nil {
		return nil, err
	}
	out := []target{{project: project, resource: authz.PolicyResource(p, g.runsCode), name: "policy/" + p.GetName()}}
	if old, ok := g.watcher.Policy(p.GetName()); ok && old.GetProject() != project {
		out = append(out, target{project: old.GetProject(), resource: authz.PolicyResource(old, g.runsCode), name: "policy/" + p.GetName()})
	}
	return out, nil
}

func rolesOf(m map[string]*evalsiv1alpha1.RoleList) map[string][]string {
	out := map[string][]string{}
	for k, v := range m {
		out[k] = v.GetRoles()
	}
	return out
}

// installWide maps "*" (every project) to the empty project, which only
// owners and roles bound in every project can act on.
func installWide(project string) string {
	if project == "*" {
		return ""
	}
	return project
}

func (g *gate) keyTargets(name string, roles map[string][]string) ([]target, error) {
	if len(roles) == 0 {
		return []target{{project: authz.DefaultProject, name: "apikey/" + name}}, nil
	}
	var out []target
	for project := range roles {
		if project != "*" {
			if _, err := g.project(project); err != nil {
				return nil, err
			}
		}
		out = append(out, target{project: installWide(project), name: "apikey/" + name,
			resource: map[string]any{"key": map[string]any{"name": name}}})
	}
	return out, nil
}

func (g *gate) roleTarget(project string, r *evalsiv1alpha1.Role) ([]target, error) {
	if project != "" {
		if _, err := g.project(project); err != nil {
			return nil, err
		}
	}
	perms := make([]any, 0, len(r.GetPermissions()))
	for _, p := range r.GetPermissions() {
		perms = append(perms, p)
	}
	return []target{{project: project, name: "role/" + r.GetName(), resource: map[string]any{
		"role": map[string]any{"name": r.GetName(), "permissions": perms, "condition": r.GetCondition()},
	}}}, nil
}

func (g *gate) bindingTarget(b *evalsiv1alpha1.RoleBinding) ([]target, error) {
	project := b.GetProject()
	if project == "" {
		project = authz.DefaultProject
	}
	if project != "*" {
		if _, err := g.project(project); err != nil {
			return nil, err
		}
	}
	return []target{{project: installWide(project), name: "binding/" + project + "/" + b.GetRole(), resource: map[string]any{
		"binding": map[string]any{"role": b.GetRole(), "subject": b.GetSubject()},
	}}}, nil
}

func requestID(h http.Header) string {
	if id := h.Get("X-Request-Id"); id != "" && len(id) <= 128 {
		return id
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// unauthenticated is a 401 with a Bearer challenge. gRPC sends error
// metadata as trailers, where the challenge header is not allowed, so it is
// added for the Connect protocol (and REST) only.
func unauthenticated(err error, protocol string) error {
	ce := connect.NewError(connect.CodeUnauthenticated, err)
	if protocol == connect.ProtocolConnect {
		ce.Meta().Set("WWW-Authenticate", `Bearer realm="evalsi"`)
	}
	return ce
}

// authenticate turns the middleware's result into a checker on the context,
// or an Unauthenticated error.
func (g *gate) authenticate(ctx context.Context, procedure, protocol, source string, header http.Header) (context.Context, *authz.Checker, error) {
	res := auth.FromContext(ctx)
	if res.Err != nil {
		return ctx, nil, unauthenticated(res.Err, protocol)
	}
	header = header.Clone()
	header.Set("X-Request-Id", requestID(header))
	chk := &authz.Checker{
		Engine: g.engine, Principal: auth.PrincipalFrom(ctx), RunsCode: g.runsCode,
		Meta: authz.Request{Procedure: procedure, Protocol: protocol, Headers: header, Source: source},
	}
	return authz.WithChecker(ctx, chk), chk, nil
}

// decide checks one target and audits the outcome when it must.
func (g *gate) decide(ctx context.Context, chk *authz.Checker, action string, t target, audit bool) authz.Decision {
	req := chk.Meta
	req.Action, req.Project, req.Resource = action, t.project, t.resource
	d := g.engine.Decide(ctx, chk.Principal, req)
	chk.Decision = d
	if !d.Allowed || audit {
		g.auditor.Record(ctx, &evalsiv1alpha1.AuditEvent{
			Principal: chk.Principal.ID(), Action: action, Project: t.project, Resource: t.name,
			Allowed: d.Allowed, Reason: d.Reason, Procedure: req.Procedure,
			RequestId: req.Headers.Get("X-Request-Id"), Source: req.Source,
		})
	}
	return d
}

func permissionDenied(action string, t target) error {
	where := fmt.Sprintf(" in project %q", t.project)
	if t.project == "" {
		where = ""
	}
	return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("%s on %s%s is not allowed (or it does not exist)", action, t.name, where))
}

// check authorizes one call's request message.
func (g *gate) check(ctx context.Context, chk *authz.Checker, procedure string, msg any) error {
	rule, ok := g.rules[procedure]
	if !ok {
		g.log.Error("no access rule for procedure; denying", "procedure", procedure)
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("%s has no access rule", procedure))
	}
	perm, _ := authz.Lookup(rule.action)
	switch {
	case !g.engine.Enabled():
		return nil
	case rule.filtered:
		return nil
	case rule.anyProject:
		for _, p := range append([]string{authz.DefaultProject}, projectNames(g.engine)...) {
			if g.engine.Allowed(ctx, chk.Principal, authz.Request{Action: rule.action, Project: p, Procedure: procedure, Headers: chk.Meta.Headers, Source: chk.Meta.Source, Protocol: chk.Meta.Protocol}) {
				return nil
			}
		}
		t := target{name: procedure}
		g.decide(ctx, chk, rule.action, t, false)
		return permissionDenied(rule.action, t)
	}
	targets := []target{{}}
	if rule.resolve != nil {
		var err error
		if targets, err = rule.resolve(ctx, msg); err != nil {
			return err
		}
	}
	for _, t := range targets {
		action, audited := rule.action, perm.Audited && !rule.serviceAudits
		if t.action != "" {
			p, _ := authz.Lookup(t.action)
			action, audited = t.action, p.Audited
		}
		if d := g.decide(ctx, chk, action, t, audited); !d.Allowed {
			return permissionDenied(action, t)
		}
	}
	return nil
}

// datasetTargets are what a run's dataset reads beyond the run itself: the
// project's traces, an earlier run, or a dataset promoted from another project.
func (g *gate) datasetTargets(ctx context.Context, project string, src *evalsiv1alpha1.DatasetSource) ([]target, error) {
	switch s := src.GetSource().(type) {
	case *evalsiv1alpha1.DatasetSource_Traces:
		return []target{{project: project, name: "traces/" + project, action: "traces.read",
			resource: map[string]any{"traces": map[string]any{"service": s.Traces.GetService(), "policy": s.Traces.GetPolicy()}}}}, nil
	case *evalsiv1alpha1.DatasetSource_Run:
		ts, err := g.runTargets(ctx, s.Run.GetRunId())
		for i := range ts {
			ts[i].action = "runs.read"
		}
		return ts, err
	}
	path := src.GetPath()
	if uri := src.GetUri(); uri != "" {
		_, rest, _ := strings.Cut(uri, "://")
		path, _, _ = strings.Cut(rest, "?")
	}
	if from := datasets.ProjectOf(path); from != "" && from != project {
		return []target{{project: from, name: "dataset/" + path, action: "runs.read", resource: map[string]any{"dataset": map[string]any{"path": path}}}}, nil
	}
	return nil, nil
}

func projectNames(e *authz.Engine) []string {
	var out []string
	for _, p := range e.Projects() {
		if p.Name != authz.DefaultProject {
			out = append(out, p.Name)
		}
	}
	return out
}

// interceptor is the Connect side of the gate.
func (g *gate) interceptor() connect.Interceptor { return gateInterceptor{g} }

type gateInterceptor struct{ g *gate }

func (i gateInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if req.Spec().IsClient {
			return next(ctx, req)
		}
		ctx, chk, err := i.g.authenticate(ctx, req.Spec().Procedure, req.Peer().Protocol, req.Peer().Addr, req.Header())
		if err != nil {
			return nil, err
		}
		if err := i.g.check(ctx, chk, req.Spec().Procedure, req.Any()); err != nil {
			return nil, err
		}
		return next(ctx, req)
	}
}

func (i gateInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

// WrapStreamingHandler authenticates before anything is read, and authorizes
// on the first message: the request of a server stream, or the config
// message that opens EvaluateStream.
func (i gateInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		proc := conn.Spec().Procedure
		ctx, chk, err := i.g.authenticate(ctx, proc, conn.Peer().Protocol, conn.Peer().Addr, conn.RequestHeader())
		if err != nil {
			return err
		}
		return next(ctx, &gatedConn{StreamingHandlerConn: conn, check: func(msg any) error {
			return i.g.check(ctx, chk, proc, msg)
		}})
	}
}

type gatedConn struct {
	connect.StreamingHandlerConn
	check   func(any) error
	checked bool
	err     error
}

func (c *gatedConn) Receive(msg any) error {
	if c.err != nil {
		return c.err
	}
	if err := c.StreamingHandlerConn.Receive(msg); err != nil {
		return err
	}
	if !c.checked {
		c.checked = true
		c.err = c.check(msg)
	}
	return c.err
}

// guardHTTP protects a plain HTTP route. With filtered, the handler checks
// per item (OTLP assigns each resource's project); otherwise action must be
// allowed install-wide.
// authorizeTool decides whether the MCP caller may call a tool
// (mcp.tools.call, with mcp.tool.name for CEL rules). Listing does not
// audit: tools/list asks about every tool.
func (g *gate) authorizeTool(r *http.Request, tool string, listing bool) error {
	ctx, chk, err := g.authenticate(r.Context(), "MCP tools/call", "mcp", r.RemoteAddr, r.Header)
	if err != nil {
		return err
	}
	if !g.engine.Enabled() {
		return nil
	}
	chk.Meta.MCP = map[string]any{"tool": map[string]any{"name": tool}}
	t := target{name: "mcp/tool/" + tool}
	if listing {
		req := chk.Meta
		req.Action = "mcp.tools.call"
		if !g.engine.Allowed(ctx, chk.Principal, req) {
			return permissionDenied("mcp.tools.call", t)
		}
		return nil
	}
	if d := g.decide(ctx, chk, "mcp.tools.call", t, false); !d.Allowed {
		return permissionDenied("mcp.tools.call", t)
	}
	return nil
}

func (g *gate) guardHTTP(action string, filtered bool, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, chk, err := g.authenticate(r.Context(), r.Method+" "+r.URL.Path, "http", r.RemoteAddr, r.Header)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="evalsi"`)
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		if g.engine.Enabled() && !filtered {
			if d := g.decide(ctx, chk, action, target{name: r.URL.Path}, false); !d.Allowed {
				http.Error(w, "permission denied: "+action, http.StatusForbidden)
				return
			}
		}
		h.ServeHTTP(w, r.WithContext(ctx))
	})
}

// assignTrace decides an OTLP resource's project and labels. The project is
// the resource attribute evalsi.project when the caller may write there;
// otherwise the one project the credential may ingest into; otherwise the
// default project. Labels from the credential override resource labels, so a
// collector cannot relabel what its key stamps.
func (g *gate) assignTrace(ctx context.Context, res ingest.Attrs) (string, map[string]string, error) {
	labels := ingest.ResourceLabels(res)
	chk := authz.CheckerFrom(ctx)
	if chk == nil {
		return authz.DefaultProject, labels, nil
	}
	for k, v := range chk.Principal.Labels {
		labels[k] = v
	}
	project, _ := res["evalsi.project"].(string)
	if project == "" {
		project = authz.DefaultProject
		if ps := authz.Projects(ctx, "traces.write"); len(ps) == 1 {
			project = ps[0]
		}
	}
	if !g.engine.Enabled() {
		return project, labels, nil
	}
	if !g.engine.ProjectExists(project) {
		return "", nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("unknown project %q", project))
	}
	service, _ := res["service.name"].(string)
	t := target{project: project, resource: authz.IngestResource(service, labels), name: "traces/" + service}
	if d := g.decide(ctx, chk, "traces.write", t, false); !d.Allowed {
		return "", nil, permissionDenied("traces.write", t)
	}
	return project, labels, nil
}
