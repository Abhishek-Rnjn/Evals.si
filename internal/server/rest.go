package server

import (
	"net/http"

	"connectrpc.com/vanguard"
	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1/evalsiv1alpha1connect"
)

// REST-style routes for clients that want plain HTTP and JSON, served by
// Vanguard transcoding onto the same handlers as gRPC and Connect.
// EvaluateStream (bidirectional) stays on gRPC and Connect.
//
//	POST   /v1alpha1/evaluate                  EvaluationService.Evaluate
//	POST   /v1alpha1/rewards:score             RewardService.ScoreRewards
//	GET    /v1alpha1/evaluators                CatalogService.ListEvaluators
//	GET    /v1alpha1/credentials               CatalogService.ListCredentials (?project=)
//	POST   /v1alpha1/runs                      RunService.CreateRun
//	GET    /v1alpha1/runs                      RunService.ListRuns (?project=&page_size=&page_token=)
//	GET    /v1alpha1/runs/{id}                 RunService.GetRun
//	POST   /v1alpha1/runs/{id}:cancel          RunService.CancelRun
//	POST   /v1alpha1/runs/{id}:resume          RunService.ResumeRun
//	GET    /v1alpha1/runs/{run_id}/results     RunService.ListRunResults
//	POST   /v1alpha1/runs:compare              RunService.CompareRuns
//	POST   /v1alpha1/runs/{run_id}:promote     RunService.PromoteResults
//	POST   /v1alpha1/runs:shadow               RunService.CreateShadowReplay
//	POST   /v1alpha1/policies                  MonitorService.ApplyPolicy (body: the policy)
//	GET    /v1alpha1/policies                  MonitorService.ListPolicies
//	DELETE /v1alpha1/policies/{name}           MonitorService.DeletePolicy
//	GET    /v1alpha1/policies/{name}/stats     MonitorService.GetPolicyStats
//	GET    /v1alpha1/traces                    TraceService.ListTraces
//	GET    /v1alpha1/traces/{trace_id}         TraceService.GetTrace
//	GET    /v1alpha1/traces/{trace_id}/scores  TraceService.ListScores (?project=&policy=&evaluator=)
//	GET    /v1alpha1/scores                    TraceService.ListScores (?project=&service=&labels=k=v&policy=&evaluator=&since=&page_size=&page_token=)
//	GET    /v1alpha1/whoami                    AuthService.WhoAmI
//	GET    /v1alpha1/projects                  AuthService.ListProjects
//	POST   /v1alpha1/projects                  AuthService.CreateProject
//	GET    /v1alpha1/apikeys                   AuthService.ListAPIKeys
//	POST   /v1alpha1/apikeys                   AuthService.CreateAPIKey
//	POST   /v1alpha1/apikeys/{name}:revoke     AuthService.RevokeAPIKey
//	GET    /v1alpha1/roles                     AuthService.ListRoles
//	POST   /v1alpha1/roles                     AuthService.CreateRole (body: the role)
//	POST   /v1alpha1/roles:update              AuthService.UpdateRole (body: the role)
//	DELETE /v1alpha1/roles/{name}              AuthService.DeleteRole (?project=)
//	GET    /v1alpha1/permissions               AuthService.ListPermissions
//	GET    /v1alpha1/bindings                  AuthService.ListBindings
//	POST   /v1alpha1/bindings                  AuthService.CreateBinding (body: the binding)
//	POST   /v1alpha1/bindings:delete           AuthService.DeleteBinding (body: the binding)
//	GET    /v1alpha1/audit                     AuthService.ListAuditEvents
//	POST   /v1alpha1/queues                    AnnotationService.CreateQueue (body: the queue)
//	GET    /v1alpha1/queues                    AnnotationService.ListQueues (?project=)
//	GET    /v1alpha1/queues/{name}             AnnotationService.GetQueue (?project=)
//	DELETE /v1alpha1/queues/{name}             AnnotationService.DeleteQueue (?project=)
//	POST   /v1alpha1/queues/{queue}/items      AnnotationService.AddItems
//	POST   /v1alpha1/queues/{queue}:next       AnnotationService.NextItem
//	POST   /v1alpha1/queues/{queue}/annotations AnnotationService.SubmitAnnotation
//	GET    /v1alpha1/queues/{queue}/annotations AnnotationService.ListAnnotations
//	GET    /v1alpha1/queues/{queue}/stats      AnnotationService.SummarizeQueue
//	POST   /v1alpha1/guardrails                GuardrailService.ApplyGuardrail (body: the guardrail)
//	GET    /v1alpha1/guardrails                GuardrailService.ListGuardrails (?project=)
//	GET    /v1alpha1/guardrails/{name}         GuardrailService.GetGuardrail (?project=)
//	DELETE /v1alpha1/guardrails/{name}         GuardrailService.DeleteGuardrail (?project=)
//	POST   /v1alpha1/guardrails/{guardrail}:check GuardrailService.Check
//	POST   /v1alpha1/sources                   SourceService.ApplySource (body: the source; ?validate_only=true)
//	GET    /v1alpha1/sources                   SourceService.ListSources (?project=)
//	GET    /v1alpha1/sources/{name}            SourceService.GetSource (?project=)
//	DELETE /v1alpha1/sources/{name}            SourceService.DeleteSource (?project=)
//	POST   /v1alpha1/sources/{name}:pause      SourceService.PauseSource
//	POST   /v1alpha1/sources/{name}:resume     SourceService.ResumeSource
//	POST   /v1alpha1/sources/{name}:backfill   SourceService.BackfillSource (body: {project, since})
//	POST   /v1alpha1/webhooks                  WebhookService.ApplyWebhook (body: the webhook)
//	GET    /v1alpha1/webhooks                  WebhookService.ListWebhooks (?project=)
//	GET    /v1alpha1/webhooks/{name}           WebhookService.GetWebhook (?project=)
//	DELETE /v1alpha1/webhooks/{name}           WebhookService.DeleteWebhook (?project=)
//	POST   /v1alpha1/webhooks/{name}:test      WebhookService.TestWebhook
//	GET    /v1alpha1/webhooks/{name}/deliveries WebhookService.ListWebhookDeliveries (?project=&limit=)
func restRules() []*annotations.HttpRule {
	rule := func(method, verb, path, body string) *annotations.HttpRule {
		r := &annotations.HttpRule{Selector: "evalsi.v1alpha1." + method, Body: body}
		switch verb {
		case http.MethodGet:
			r.Pattern = &annotations.HttpRule_Get{Get: path}
		case http.MethodPost:
			r.Pattern = &annotations.HttpRule_Post{Post: path}
		case http.MethodDelete:
			r.Pattern = &annotations.HttpRule_Delete{Delete: path}
		}
		return r
	}
	const v = "/v1alpha1"
	return []*annotations.HttpRule{
		rule("EvaluationService.Evaluate", http.MethodPost, v+"/evaluate", "*"),
		rule("RewardService.ScoreRewards", http.MethodPost, v+"/rewards:score", "*"),
		rule("CatalogService.ListEvaluators", http.MethodGet, v+"/evaluators", ""),
		rule("CatalogService.ListCredentials", http.MethodGet, v+"/credentials", ""),
		rule("RunService.CreateRun", http.MethodPost, v+"/runs", "*"),
		rule("RunService.ListRuns", http.MethodGet, v+"/runs", ""),
		rule("RunService.GetRun", http.MethodGet, v+"/runs/{id}", ""),
		rule("RunService.CancelRun", http.MethodPost, v+"/runs/{id}:cancel", "*"),
		rule("RunService.ResumeRun", http.MethodPost, v+"/runs/{id}:resume", "*"),
		rule("RunService.ListRunResults", http.MethodGet, v+"/runs/{run_id}/results", ""),
		rule("RunService.CompareRuns", http.MethodPost, v+"/runs:compare", "*"),
		rule("RunService.PromoteResults", http.MethodPost, v+"/runs/{run_id}:promote", "*"),
		rule("RunService.CreateShadowReplay", http.MethodPost, v+"/runs:shadow", "*"),
		rule("MonitorService.ApplyPolicy", http.MethodPost, v+"/policies", "policy"),
		rule("MonitorService.ListPolicies", http.MethodGet, v+"/policies", ""),
		rule("MonitorService.DeletePolicy", http.MethodDelete, v+"/policies/{name}", ""),
		rule("MonitorService.GetPolicyStats", http.MethodGet, v+"/policies/{name}/stats", ""),
		rule("TraceService.ListTraces", http.MethodGet, v+"/traces", ""),
		rule("TraceService.GetTrace", http.MethodGet, v+"/traces/{trace_id}", ""),
		rule("TraceService.ListScores", http.MethodGet, v+"/traces/{trace_id}/scores", ""),
		rule("TraceService.ListScores", http.MethodGet, v+"/scores", ""),
		rule("AuthService.WhoAmI", http.MethodGet, v+"/whoami", ""),
		rule("AuthService.ListProjects", http.MethodGet, v+"/projects", ""),
		rule("AuthService.CreateProject", http.MethodPost, v+"/projects", "*"),
		rule("AuthService.ListAPIKeys", http.MethodGet, v+"/apikeys", ""),
		rule("AuthService.CreateAPIKey", http.MethodPost, v+"/apikeys", "*"),
		rule("AuthService.RevokeAPIKey", http.MethodPost, v+"/apikeys/{name}:revoke", "*"),
		rule("AuthService.ListRoles", http.MethodGet, v+"/roles", ""),
		rule("AuthService.CreateRole", http.MethodPost, v+"/roles", "role"),
		rule("AuthService.UpdateRole", http.MethodPost, v+"/roles:update", "role"),
		rule("AuthService.DeleteRole", http.MethodDelete, v+"/roles/{name}", ""),
		rule("AuthService.ListPermissions", http.MethodGet, v+"/permissions", ""),
		rule("AuthService.ListBindings", http.MethodGet, v+"/bindings", ""),
		rule("AuthService.CreateBinding", http.MethodPost, v+"/bindings", "binding"),
		rule("AuthService.DeleteBinding", http.MethodPost, v+"/bindings:delete", "binding"),
		rule("AuthService.ListAuditEvents", http.MethodGet, v+"/audit", ""),
		rule("AnnotationService.CreateQueue", http.MethodPost, v+"/queues", "queue"),
		rule("AnnotationService.ListQueues", http.MethodGet, v+"/queues", ""),
		rule("AnnotationService.GetQueue", http.MethodGet, v+"/queues/{name}", ""),
		rule("AnnotationService.DeleteQueue", http.MethodDelete, v+"/queues/{name}", ""),
		rule("AnnotationService.AddItems", http.MethodPost, v+"/queues/{queue}/items", "*"),
		rule("AnnotationService.NextItem", http.MethodPost, v+"/queues/{queue}:next", "*"),
		rule("AnnotationService.SubmitAnnotation", http.MethodPost, v+"/queues/{queue}/annotations", "*"),
		rule("AnnotationService.ListAnnotations", http.MethodGet, v+"/queues/{queue}/annotations", ""),
		rule("AnnotationService.SummarizeQueue", http.MethodGet, v+"/queues/{queue}/stats", ""),
		rule("GuardrailService.ApplyGuardrail", http.MethodPost, v+"/guardrails", "guardrail"),
		rule("GuardrailService.ListGuardrails", http.MethodGet, v+"/guardrails", ""),
		rule("GuardrailService.GetGuardrail", http.MethodGet, v+"/guardrails/{name}", ""),
		rule("GuardrailService.DeleteGuardrail", http.MethodDelete, v+"/guardrails/{name}", ""),
		rule("GuardrailService.Check", http.MethodPost, v+"/guardrails/{guardrail}:check", "*"),
		rule("SourceService.ApplySource", http.MethodPost, v+"/sources", "source"),
		rule("SourceService.ListSources", http.MethodGet, v+"/sources", ""),
		rule("SourceService.GetSource", http.MethodGet, v+"/sources/{name}", ""),
		rule("SourceService.DeleteSource", http.MethodDelete, v+"/sources/{name}", ""),
		rule("SourceService.PauseSource", http.MethodPost, v+"/sources/{name}:pause", "*"),
		rule("SourceService.ResumeSource", http.MethodPost, v+"/sources/{name}:resume", "*"),
		rule("SourceService.BackfillSource", http.MethodPost, v+"/sources/{name}:backfill", "*"),
		rule("WebhookService.ApplyWebhook", http.MethodPost, v+"/webhooks", "webhook"),
		rule("WebhookService.ListWebhooks", http.MethodGet, v+"/webhooks", ""),
		rule("WebhookService.GetWebhook", http.MethodGet, v+"/webhooks/{name}", ""),
		rule("WebhookService.DeleteWebhook", http.MethodDelete, v+"/webhooks/{name}", ""),
		rule("WebhookService.TestWebhook", http.MethodPost, v+"/webhooks/{name}:test", "*"),
		rule("WebhookService.ListWebhookDeliveries", http.MethodGet, v+"/webhooks/{name}/deliveries", ""),
	}
}

// restHandler transcodes REST calls onto the services' Connect handlers.
func restHandler(handlers map[string]http.Handler) (http.Handler, error) {
	names := []string{
		evalsiv1alpha1connect.EvaluationServiceName,
		evalsiv1alpha1connect.CatalogServiceName,
		evalsiv1alpha1connect.RewardServiceName,
		evalsiv1alpha1connect.RunServiceName,
		evalsiv1alpha1connect.MonitorServiceName,
		evalsiv1alpha1connect.TraceServiceName,
		evalsiv1alpha1connect.AuthServiceName,
		evalsiv1alpha1connect.AnnotationServiceName,
		evalsiv1alpha1connect.GuardrailServiceName,
		evalsiv1alpha1connect.WebhookServiceName,
		evalsiv1alpha1connect.SourceServiceName,
	}
	services := make([]*vanguard.Service, 0, len(names))
	for _, name := range names {
		schema, err := restSchema(name)
		if err != nil {
			return nil, err
		}
		services = append(services, vanguard.NewServiceWithSchema(schema, handlers[name],
			vanguard.WithTargetProtocols(vanguard.ProtocolConnect)))
	}
	return vanguard.NewTranscoder(services, vanguard.WithRules(restRules()...))
}

// restSchema is the service without its client-streaming methods, which REST
// cannot carry. Vanguard matches rule selectors by prefix, so without this the
// Evaluate rule would also claim EvaluateStream.
func restSchema(name string) (protoreflect.ServiceDescriptor, error) {
	d, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(name))
	if err != nil {
		return nil, err
	}
	svc := d.(protoreflect.ServiceDescriptor)
	fdp := protodesc.ToFileDescriptorProto(svc.ParentFile())
	for _, s := range fdp.GetService() {
		if s.GetName() != string(svc.Name()) {
			continue
		}
		kept := s.Method[:0]
		for _, m := range s.GetMethod() {
			if !m.GetClientStreaming() {
				kept = append(kept, m)
			}
		}
		s.Method = kept
	}
	file, err := protodesc.NewFile(fdp, protoregistry.GlobalFiles)
	if err != nil {
		return nil, err
	}
	return file.Services().ByName(svc.Name()), nil
}
