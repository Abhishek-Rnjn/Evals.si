package watch

import (
	"os"
	"testing"

	collectortracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/ingest"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

// A trace query sees the same tools and models a policy selector does, also
// for a CrewAI trace recorded by MLflow, which has no tool spans (the tool is
// read from the model's request) and names its model in mlflow.llm.model.
func TestTraceFilterSeesToolsAndModelsAsPoliciesDo(t *testing.T) {
	raw, err := os.ReadFile("../ingest/testdata/frameworks/crewai-mlflow.otlp.json")
	if err != nil {
		t.Fatal(err)
	}
	req := &collectortracepb.ExportTraceServiceRequest{}
	if err := ingest.UnmarshalOTLPJSON(raw, req); err != nil {
		t.Fatal(err)
	}
	var spans []ingest.Span
	for _, rs := range req.ResourceSpans {
		spans = append(spans, ingest.SpansOfResource(rs, "", nil)...)
	}
	rec, info := ingest.ToRecord(ingest.Trace{TraceID: "crewai", Spans: spans})
	if len(info.Tools) != 1 || info.Tools[0] != "search_docs" || len(info.Models) != 1 || info.Models[0] != "gpt-4o-mini" {
		t.Fatalf("policy sees tools %v, models %v", info.Tools, info.Models)
	}
	f, err := CompileTraceFilter(`"search_docs" in tools && "gpt-4o-mini" in models`)
	if err != nil {
		t.Fatal(err)
	}
	if !f.Match(store.StoredTrace{Summary: &evalsiv1alpha1.TraceSummary{}, Record: rec}, nil) {
		t.Error("the trace query does not see the tool or the model")
	}
}
