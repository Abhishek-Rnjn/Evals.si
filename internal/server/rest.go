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
//	GET    /v1alpha1/evaluators                CatalogService.ListEvaluators
//	POST   /v1alpha1/runs                      RunService.CreateRun
//	GET    /v1alpha1/runs                      RunService.ListRuns (?project=&page_size=&page_token=)
//	GET    /v1alpha1/runs/{id}                 RunService.GetRun
//	POST   /v1alpha1/runs/{id}:cancel          RunService.CancelRun
//	POST   /v1alpha1/runs/{id}:resume          RunService.ResumeRun
//	GET    /v1alpha1/runs/{run_id}/results     RunService.ListRunResults
//	POST   /v1alpha1/runs:compare              RunService.CompareRuns
//	POST   /v1alpha1/policies                  MonitorService.ApplyPolicy (body: the policy)
//	GET    /v1alpha1/policies                  MonitorService.ListPolicies
//	DELETE /v1alpha1/policies/{name}           MonitorService.DeletePolicy
//	GET    /v1alpha1/policies/{name}/stats     MonitorService.GetPolicyStats
//	GET    /v1alpha1/traces                    TraceService.ListTraces
//	GET    /v1alpha1/traces/{trace_id}         TraceService.GetTrace
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
		rule("CatalogService.ListEvaluators", http.MethodGet, v+"/evaluators", ""),
		rule("RunService.CreateRun", http.MethodPost, v+"/runs", "*"),
		rule("RunService.ListRuns", http.MethodGet, v+"/runs", ""),
		rule("RunService.GetRun", http.MethodGet, v+"/runs/{id}", ""),
		rule("RunService.CancelRun", http.MethodPost, v+"/runs/{id}:cancel", "*"),
		rule("RunService.ResumeRun", http.MethodPost, v+"/runs/{id}:resume", "*"),
		rule("RunService.ListRunResults", http.MethodGet, v+"/runs/{run_id}/results", ""),
		rule("RunService.CompareRuns", http.MethodPost, v+"/runs:compare", "*"),
		rule("MonitorService.ApplyPolicy", http.MethodPost, v+"/policies", "policy"),
		rule("MonitorService.ListPolicies", http.MethodGet, v+"/policies", ""),
		rule("MonitorService.DeletePolicy", http.MethodDelete, v+"/policies/{name}", ""),
		rule("MonitorService.GetPolicyStats", http.MethodGet, v+"/policies/{name}/stats", ""),
		rule("TraceService.ListTraces", http.MethodGet, v+"/traces", ""),
		rule("TraceService.GetTrace", http.MethodGet, v+"/traces/{trace_id}", ""),
	}
}

// restHandler transcodes REST calls onto the services' Connect handlers.
func restHandler(handlers map[string]http.Handler) (http.Handler, error) {
	names := []string{
		evalsiv1alpha1connect.EvaluationServiceName,
		evalsiv1alpha1connect.CatalogServiceName,
		evalsiv1alpha1connect.RunServiceName,
		evalsiv1alpha1connect.MonitorServiceName,
		evalsiv1alpha1connect.TraceServiceName,
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
