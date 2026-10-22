package ingest

import "testing"

// The GenAI semconv schema carries a tool result in the part's "response"
// (any JSON value); "content" is only what older emitters wrote.
func TestGenAIToolCallResponseReadsResponse(t *testing.T) {
	cases := map[string]string{
		`[{"role":"tool","parts":[{"type":"tool_call_response","id":"call_1","response":"72F and sunny"}]}]`: "72F and sunny",
		`[{"role":"tool","parts":[{"type":"tool_call_response","id":"call_1","response":{"temp":72}}]}]`:     `{"temp":72}`,
		`[{"role":"tool","parts":[{"type":"tool_call_response","id":"call_1","content":"legacy"}]}]`:         "legacy",
	}
	for raw, want := range cases {
		msgs := genAIMessages(raw, false)
		if len(msgs) != 1 || msgs[0].GetContent() != want || msgs[0].GetToolCallId() != "call_1" {
			t.Errorf("%s: got %v, want content %q", raw, msgs, want)
		}
	}
}
