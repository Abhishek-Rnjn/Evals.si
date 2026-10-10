package source

import (
	"encoding/hex"
	"strings"
	"testing"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/abhishek-rnjn/evals.si/internal/ingest"
)

func str(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

// A studio's trace, as the MLflow connector hands it over: the workflow is a
// tag, the question a root attribute, and the conventions find the answer.
func studioTrace() ingest.Trace {
	id := make([]byte, 16)
	id[0] = 7
	sp := &tracepb.Span{
		TraceId: id, SpanId: []byte{1, 2, 3, 4, 5, 6, 7, 8}, Name: "workflow", StartTimeUnixNano: 1, EndTimeUnixNano: 2,
		Attributes: []*commonpb.KeyValue{str("input.value", "What is 2+2?"), str("output.value", "4"), str("studio.expected", "4")},
	}
	return ingest.Trace{TraceID: hex.EncodeToString(id), Spans: []ingest.Span{{
		Span: sp, Labels: map[string]string{"source": "studio"},
		Resource: ingest.Attrs{"service.name": "studio", "mlflow.tag.agent_studio.workflow_id": "wf-42", "mlflow.tag.docs": `["a","b"]`},
	}}}
}

func TestOverridesSetFieldsAfterTheConventions(t *testing.T) {
	adjust, err := CompileOverrides(map[string]string{
		"reference":          `attributes["studio.expected"]`,
		"context":            `["doc one", "doc two"]`,
		"metadata.workflow":  `resource["mlflow.tag.agent_studio.workflow_id"]`,
		"labels.workflow":    `resource["mlflow.tag.agent_studio.workflow_id"]`,
		"output":             `output + "!"`,
		"metadata.structure": `{"steps": 2, "ok": true}`,
		// A key the trace lacks leaves the field alone rather than failing the trace.
		"metadata.missing": `resource["nope"]`,
	})
	if err != nil {
		t.Fatal(err)
	}
	tr := studioTrace()
	tr.Adjust = adjust
	rec, info := ingest.ToRecord(tr)
	if rec.GetReference().GetText() != "4" || rec.GetOutput().GetText() != "4!" || rec.GetInput().GetText() != "What is 2+2?" {
		t.Fatalf("record %v", rec)
	}
	if len(rec.GetContext()) != 2 || rec.GetContext()[1].GetText() != "doc two" {
		t.Fatalf("context %v", rec.GetContext())
	}
	if rec.GetMetadata()["workflow"].GetStringValue() != "wf-42" || rec.GetMetadata()["structure"].GetStructValue().GetFields()["steps"].GetNumberValue() != 2 {
		t.Fatalf("metadata %v", rec.GetMetadata())
	}
	if _, set := rec.GetMetadata()["missing"]; set {
		t.Fatal("a failed expression set a field")
	}
	// Labels are what policy selectors see, so they change before selection.
	if info.Labels["workflow"] != "wf-42" || info.Labels["source"] != "studio" {
		t.Fatalf("labels %v", info.Labels)
	}
}

func TestOverridesAreChecked(t *testing.T) {
	for overrides, want := range map[string]map[string]string{
		"can set":      {"usage": `"x"`},
		"takes no key": {"input.x": `"x"`},
		"name the key": {"metadata": `"x"`},
		"undeclared":   {"input": `trace.request`},
		"Syntax":       {"input": `((`},
	} {
		_, err := CompileOverrides(want)
		if err == nil || !strings.Contains(err.Error(), overrides) {
			t.Errorf("%v: got %v, want %q", want, err, overrides)
		}
	}
	if adjust, err := CompileOverrides(nil); adjust != nil || err != nil {
		t.Fatal("no overrides should be no adjuster")
	}
}
