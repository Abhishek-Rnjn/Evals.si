package ingest

import (
	"os"
	"strings"
	"testing"

	collectortracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

// testdata/deepagents-research.otlp.pb is a recorded OTLP export of one run of
// the Deep Agents demo (examples/demo/deepagents) with MLflow's LangChain
// autolog and the OTLP exporter, answering a research question against the
// mock model. examples/demo/deepagents/record_fixture.py records it again.
func TestDeepAgentsMLflowTrace(t *testing.T) {
	raw, err := os.ReadFile("testdata/deepagents-research.otlp.pb")
	if err != nil {
		t.Fatal(err)
	}
	req := &collectortracepb.ExportTraceServiceRequest{}
	if err := proto.Unmarshal(raw, req); err != nil {
		t.Fatal(err)
	}
	var spans []Span
	for _, rs := range req.ResourceSpans {
		spans = append(spans, SpansOfResource(rs, "", nil)...)
	}
	rec, info := ToRecord(Trace{TraceID: "deepagents", Spans: spans})

	// The record is the question and the final report, not the graph's state.
	in := recordText(rec.GetInput())
	out := recordText(rec.GetOutput())
	if !strings.Contains(in, "Research how retrieval-augmented generation is evaluated") {
		t.Errorf("input %q", in)
	}
	if !strings.HasPrefix(out, "# Findings") || !strings.Contains(out, "[2] rag-evaluation") {
		t.Errorf("output %q", out)
	}

	// Model calls and the tool call are typed steps; graph nodes stay generic.
	var llm, tool []*evalsiv1alpha1.Step
	for _, s := range rec.GetTrajectory().GetSteps() {
		switch s.GetType() {
		case evalsiv1alpha1.StepType_STEP_TYPE_LLM:
			llm = append(llm, s)
		case evalsiv1alpha1.StepType_STEP_TYPE_TOOL:
			tool = append(tool, s)
		}
	}
	if len(llm) != 2 || len(tool) != 1 || len(rec.GetTrajectory().GetSteps()) != 8 {
		t.Fatalf("%d model calls, %d tool calls, %d steps", len(llm), len(tool), len(rec.GetTrajectory().GetSteps()))
	}
	if tool[0].GetName() != "search_docs" || !strings.Contains(tool[0].GetInput().GetJson().String(), "retrieval-augmented generation evaluation") {
		t.Errorf("tool step %v", tool[0])
	}
	// The first call asks for the tool; the second answers.
	asked := llm[0].GetOutput().GetMessages().GetMessages()
	if len(asked) != 1 || len(asked[0].GetToolCalls()) != 1 || asked[0].GetToolCalls()[0].GetName() != "search_docs" ||
		!strings.Contains(asked[0].GetToolCalls()[0].GetArguments(), "retrieval-augmented generation evaluation") {
		t.Errorf("first model call %v", asked)
	}
	if got := recordText(llm[1].GetOutput()); !strings.HasPrefix(got, "# Findings") {
		t.Errorf("second model call %q", got)
	}
	if llm[0].GetUsage().GetInputTokens() != 81 || llm[0].GetUsage().GetOutputTokens() != 41 ||
		llm[1].GetUsage().GetInputTokens() != 283 || llm[1].GetUsage().GetOutputTokens() != 69 {
		t.Errorf("usage %v %v", llm[0].GetUsage(), llm[1].GetUsage())
	}
	if rec.GetUsage().GetInputTokens() != 364 || rec.GetUsage().GetOutputTokens() != 110 {
		t.Errorf("record usage %v", rec.GetUsage())
	}
	if strings.Join(info.Tools, ",") != "search_docs" || strings.Join(info.Models, ",") != "mock" || info.Error {
		t.Errorf("info %+v", info)
	}
}

func recordText(c *evalsiv1alpha1.Content) string {
	if m := c.GetMessages().GetMessages(); len(m) > 0 {
		for i := len(m) - 1; i >= 0; i-- {
			if m[i].GetRole() == "assistant" {
				return m[i].GetContent()
			}
		}
		return m[len(m)-1].GetContent()
	}
	return c.GetText()
}
