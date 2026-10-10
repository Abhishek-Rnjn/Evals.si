package ingest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	collectortracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

// The fixtures in testdata/frameworks are OTLP exports recorded from real
// agent frameworks (tests/frameworks has the recorders and their pinned
// versions). Each one runs the same task against the demo's mock model: the
// research question, one search_docs call, then a fixed report. So every
// framework, however it is instrumented, must come out as the same record.
const (
	fixtureQuestion = "Research how retrieval-augmented generation is evaluated and write a short report."
	fixtureArgs     = "retrieval-augmented generation evaluation"
	fixtureAnswer   = "# Findings"
	fixtureResult   = "[rag-evaluation] RAG is evaluated in two parts"
)

// What differs between fixtures: how many spans each instrumentation makes,
// and what it records of them.
type frameworkWant struct {
	llm, tool int // typed steps
	// The model's search_docs request is on the first model call's output.
	requestOnLLM bool
	// The tool step has the call's arguments and result.
	toolIO bool
	// The instrumentation records no final answer (no model output).
	noAnswer bool
	model    string
	// Summed over model calls.
	inTokens, outTokens int64
}

var frameworkFixtures = map[string]frameworkWant{
	// The agent-studio path: MLflow's CrewAI autolog sees the crew, task and
	// agent; its OpenAI autolog the model calls. Nothing records the tool, which
	// CrewAI 1.x runs inside its LLM call.
	"crewai-mlflow":        {llm: 2, requestOnLLM: true, model: "gpt-4o-mini", inTokens: 232, outTokens: 110},
	"crewai-openinference": {llm: 2, tool: 1, requestOnLLM: true, toolIO: true, model: "gpt-4o-mini", inTokens: 232, outTokens: 110},
	"crewai-openllmetry":   {llm: 2, requestOnLLM: true, model: "gpt-4o-mini", inTokens: 232, outTokens: 110},

	"langgraph-openinference": {llm: 2, tool: 1, requestOnLLM: true, toolIO: true, model: "gpt-4o-mini", inTokens: 81, outTokens: 110},
	"langgraph-langsmith":     {llm: 2, tool: 1, requestOnLLM: true, toolIO: true, model: "gpt-4o-mini", inTokens: 81, outTokens: 110},

	"openai-agents-openinference": {llm: 2, tool: 1, requestOnLLM: true, toolIO: true, model: "gpt-4o-mini", inTokens: 111, outTokens: 110},
	// OpenLLMetry's instrumentor records no model output with a chat
	// completions model, nor usage.
	"openai-agents-openllmetry": {llm: 2, tool: 1, toolIO: true, noAnswer: true, model: "gpt-4o-mini"},

	// Streamed calls: the mock reports no usage unless asked, and LlamaIndex
	// does not ask.
	"llamaindex-openinference": {llm: 2, tool: 1, requestOnLLM: true, toolIO: true, model: "gpt-4o-mini"},

	// One agent span with the whole conversation, and the tool.
	"claude-agent-sdk-openinference": {tool: 1, model: "claude-sonnet-4-5", inTokens: 568, outTokens: 113},
	// Claude Code's own traces: model calls and tools, but no model output
	// and no tool arguments.
	"claude-agent-sdk-native": {llm: 2, tool: 1, noAnswer: true, model: "claude-sonnet-4-5", inTokens: 568, outTokens: 113},
}

func loadFixture(t *testing.T, path string) Trace {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	req := &collectortracepb.ExportTraceServiceRequest{}
	if err := UnmarshalOTLPJSON(raw, req); err != nil {
		t.Fatal(err)
	}
	var spans []Span
	for _, rs := range req.ResourceSpans {
		spans = append(spans, SpansOfResource(rs, "", nil)...)
	}
	if len(spans) == 0 {
		t.Fatal("no spans")
	}
	return Trace{TraceID: strings.TrimSuffix(filepath.Base(path), ".otlp.json"), Spans: spans}
}

func TestFrameworkProfiles(t *testing.T) {
	paths, _ := filepath.Glob("testdata/frameworks/*.otlp.json")
	if len(paths) != len(frameworkFixtures) {
		t.Errorf("%d fixtures, %d expectations", len(paths), len(frameworkFixtures))
	}
	for _, path := range paths {
		name := strings.TrimSuffix(filepath.Base(path), ".otlp.json")
		t.Run(name, func(t *testing.T) {
			want, ok := frameworkFixtures[name]
			if !ok {
				t.Fatalf("no expectation for %s", name)
			}
			rec, info := ToRecord(loadFixture(t, path))

			// The record is the question and the report, as text.
			// (A framework may wrap the question in its own prompt.)
			if in := fixtureText(rec.GetInput()); !strings.Contains(in, fixtureQuestion) {
				t.Errorf("input %.300q", in)
			}
			out := fixtureText(rec.GetOutput())
			if want.noAnswer {
				if rec.GetOutput() != nil {
					t.Errorf("output %.300q, want none", out)
				}
			} else if !strings.HasPrefix(out, fixtureAnswer) || !strings.Contains(out, "[2] rag-evaluation") {
				t.Errorf("output %.300q", out)
			}

			var llm, tool []*evalsiv1alpha1.Step
			for _, s := range rec.GetTrajectory().GetSteps() {
				switch s.GetType() {
				case evalsiv1alpha1.StepType_STEP_TYPE_LLM:
					llm = append(llm, s)
				case evalsiv1alpha1.StepType_STEP_TYPE_TOOL:
					tool = append(tool, s)
				}
			}
			if len(llm) != want.llm || len(tool) != want.tool {
				t.Errorf("%d model calls, %d tool calls; want %d, %d", len(llm), len(tool), want.llm, want.tool)
			}
			if want.requestOnLLM && len(llm) > 0 {
				calls := toolCalls(llm[0].GetOutput())
				if len(calls) != 1 || !strings.HasSuffix(calls[0].GetName(), "search_docs") || !strings.Contains(calls[0].GetArguments(), fixtureArgs) {
					t.Errorf("first model call asks for %v", calls)
				}
				if got := fixtureText(llm[len(llm)-1].GetOutput()); !strings.HasPrefix(got, fixtureAnswer) {
					t.Errorf("last model call answers %.200q", got)
				}
			}
			for _, s := range tool {
				if !strings.HasSuffix(s.GetName(), "search_docs") {
					t.Errorf("tool step named %q", s.GetName())
				}
				if want.toolIO && (!strings.Contains(fixtureText(s.GetInput())+s.GetInput().GetJson().String(), fixtureArgs) ||
					!strings.Contains(fixtureText(s.GetOutput())+s.GetOutput().GetJson().String(), fixtureResult)) {
					t.Errorf("tool step input %v output %v", s.GetInput(), s.GetOutput())
				}
			}

			// Policies select on the tool and the model, whichever way they were recorded.
			if len(info.Tools) != 1 || !strings.HasSuffix(info.Tools[0], "search_docs") {
				t.Errorf("tools %v", info.Tools)
			}
			if strings.Join(info.Models, ",") != want.model {
				t.Errorf("models %v, want %s", info.Models, want.model)
			}
			if info.Error {
				t.Error("marked as an error")
			}
			if rec.GetUsage().GetInputTokens() != want.inTokens || rec.GetUsage().GetOutputTokens() != want.outTokens {
				t.Errorf("usage %d/%d, want %d/%d", rec.GetUsage().GetInputTokens(), rec.GetUsage().GetOutputTokens(), want.inTokens, want.outTokens)
			}
		})
	}
}

// fixtureText is a content's text: the last assistant message (or the last
// message) of a list, else the text.
func fixtureText(c *evalsiv1alpha1.Content) string {
	if m := c.GetMessages().GetMessages(); len(m) > 0 {
		for i := len(m) - 1; i >= 0; i-- {
			if m[i].GetRole() == "assistant" && m[i].GetContent() != "" {
				return m[i].GetContent()
			}
		}
		if len(m) == 1 || m[len(m)-1].GetRole() != "assistant" {
			return m[len(m)-1].GetContent()
		}
		return ""
	}
	return c.GetText()
}

func toolCalls(c *evalsiv1alpha1.Content) []*evalsiv1alpha1.ToolCall {
	var out []*evalsiv1alpha1.ToolCall
	for _, m := range c.GetMessages().GetMessages() {
		out = append(out, m.GetToolCalls()...)
	}
	return out
}

// A model call recorded twice (a framework's span around its client's) is
// one model call; its tokens count once, from whichever span has them.
func TestNestedModelCallsCountOnce(t *testing.T) {
	llm := func(id, parent byte, start uint64, tokens bool) Span {
		attrs := []*commonpb.KeyValue{
			{Key: "openinference.span.kind", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "LLM"}}},
		}
		if tokens {
			attrs = append(attrs,
				&commonpb.KeyValue{Key: "llm.token_count.prompt", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 10}}},
				&commonpb.KeyValue{Key: "llm.token_count.completion", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 5}}})
		}
		sp := &tracepb.Span{TraceId: []byte{1}, SpanId: []byte{id}, Name: fmt.Sprintf("call-%d", id), StartTimeUnixNano: start, EndTimeUnixNano: start + 100, Attributes: attrs}
		if parent != 0 {
			sp.ParentSpanId = []byte{parent}
		}
		return Span{Span: sp}
	}
	root := Span{Span: &tracepb.Span{TraceId: []byte{1}, SpanId: []byte{9}, Name: "agent", StartTimeUnixNano: 1, EndTimeUnixNano: 1000}}
	for name, tc := range map[string]struct {
		spans    []Span
		in, out  int64
		llmSteps int
	}{
		"both record tokens":           {spans: []Span{root, llm(1, 9, 10, true), llm(2, 1, 20, true)}, in: 10, out: 5, llmSteps: 1},
		"only the wrapper has tokens":  {spans: []Span{root, llm(1, 9, 10, true), llm(2, 1, 20, false)}, in: 10, out: 5, llmSteps: 1},
		"two wrappers, inner has none": {spans: []Span{root, llm(1, 9, 10, true), llm(2, 1, 20, true), llm(3, 2, 30, false)}, in: 10, out: 5, llmSteps: 1},
		"two separate calls":           {spans: []Span{root, llm(1, 9, 10, true), llm(2, 9, 200, true)}, in: 20, out: 10, llmSteps: 2},
		"a wrapped call and another":   {spans: []Span{root, llm(1, 9, 10, true), llm(2, 1, 20, false), llm(3, 9, 200, true)}, in: 20, out: 10, llmSteps: 2},
	} {
		t.Run(name, func(t *testing.T) {
			rec, _ := ToRecord(Trace{TraceID: "t", Spans: tc.spans})
			n := 0
			for _, s := range rec.GetTrajectory().GetSteps() {
				if s.GetType() == evalsiv1alpha1.StepType_STEP_TYPE_LLM {
					n++
				}
			}
			if n != tc.llmSteps || rec.GetUsage().GetInputTokens() != tc.in || rec.GetUsage().GetOutputTokens() != tc.out {
				t.Errorf("%d model calls, usage %d/%d; want %d, %d/%d", n, rec.GetUsage().GetInputTokens(), rec.GetUsage().GetOutputTokens(), tc.llmSteps, tc.in, tc.out)
			}
		})
	}
}
