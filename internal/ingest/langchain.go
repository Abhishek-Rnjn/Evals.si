package ingest

import (
	"encoding/json"
	"strings"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

// MLflow tracing, which LangChain and LangGraph autolog (and so Deep Agents)
// use, writes every attribute value as JSON: a string arrives quoted
// (`"CHAT_MODEL"`), and the messages of a model call, a chain or a graph are
// OpenAI-style or LangChain-serialized objects. These helpers read them.

// mlflowStr is str for an attribute MLflow wrote as a JSON string.
func (a Attrs) mlflowStr(keys ...string) string {
	s := strings.TrimSpace(a.str(keys...))
	if strings.HasPrefix(s, `"`) {
		var out string
		if json.Unmarshal([]byte(s), &out) == nil {
			return out
		}
	}
	return s
}

// mlflowTokens reads mlflow.chat.tokenUsage: {"input_tokens", "output_tokens"}.
func (a Attrs) mlflowTokens() (in, out *int64) {
	raw := a.str("mlflow.chat.tokenUsage")
	if raw == "" {
		return nil, nil
	}
	var u struct {
		In  *int64 `json:"input_tokens"`
		Out *int64 `json:"output_tokens"`
	}
	if json.Unmarshal([]byte(raw), &u) != nil {
		return nil, nil
	}
	return u.In, u.Out
}

// langchainRoles maps LangChain's message types to chat roles.
var langchainRoles = map[string]string{"human": "user", "ai": "assistant", "system": "system", "tool": "tool", "function": "tool"}

type mlflowMessage struct {
	Role       string          `json:"role"`
	Type       string          `json:"type"`
	Content    json.RawMessage `json:"content"`
	ToolCallID string          `json:"tool_call_id"`
	ToolCalls  []struct {
		ID       string          `json:"id"`
		Name     string          `json:"name"`
		Args     json.RawMessage `json:"args"`
		Function struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

func (m mlflowMessage) message() *evalsiv1alpha1.Message {
	role := m.Role
	if role == "" {
		role = langchainRoles[m.Type]
	}
	out := &evalsiv1alpha1.Message{Role: role, Content: contentText(m.Content), ToolCallId: m.ToolCallID}
	for _, tc := range m.ToolCalls {
		name, args := tc.Name, tc.Args
		if tc.Function.Name != "" {
			name, args = tc.Function.Name, tc.Function.Arguments
		}
		out.ToolCalls = append(out.ToolCalls, &evalsiv1alpha1.ToolCall{Id: tc.ID, Name: name, Arguments: rawText(args)})
	}
	return out
}

// contentText is a message's text: a string, or the text parts of a list.
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var texts []string
		for _, p := range parts {
			if p.Text != "" {
				texts = append(texts, p.Text)
			}
		}
		return strings.Join(texts, "\n")
	}
	return string(raw)
}

// messagesOf reads the messages in a span's inputs or outputs, as MLflow,
// OpenInference (input.value, output.value) and LangSmith (gen_ai.prompt,
// gen_ai.completion) write them for LangChain, LangGraph and OpenAI calls:
//   - {"messages": [...]}: a model call's input or a graph's state; LangSmith
//     nests a batch ([[...]]);
//   - {"choices": [{"message": {...}}]}: an OpenAI completion;
//   - {"generations": [[{"message": {...}}]]}: a LangChain LLMResult.
//
// A message is OpenAI-style ({"role", "content", "tool_calls"}), LangChain's
// own ({"type": "human", "content"}), its dumped form ({"type": "human",
// "data": {...}}) or its serialized form ({"lc": 1, "kwargs": {...}}). It
// returns nil for anything else.
func messagesOf(raw string) []*evalsiv1alpha1.Message {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "{") {
		return nil
	}
	var doc struct {
		Messages []json.RawMessage `json:"messages"`
		Choices  []struct {
			Message json.RawMessage `json:"message"`
		} `json:"choices"`
		Generations []json.RawMessage `json:"generations"`
	}
	if json.Unmarshal([]byte(raw), &doc) != nil {
		return nil
	}
	var items []json.RawMessage
	for _, m := range doc.Messages {
		items = append(items, flatten(m)...)
	}
	for _, c := range doc.Choices {
		items = append(items, c.Message)
	}
	for _, g := range doc.Generations {
		for _, gen := range flatten(g) {
			var x struct {
				Message json.RawMessage `json:"message"`
			}
			if json.Unmarshal(gen, &x) == nil && len(x.Message) > 0 {
				items = append(items, x.Message)
			}
		}
	}
	var out []*evalsiv1alpha1.Message
	for _, item := range items {
		m, ok := langchainMessage(item)
		if !ok {
			return nil
		}
		out = append(out, m.message())
	}
	return out
}

// flatten unnests lists: [[a, b], [c]] is a, b, c.
func flatten(raw json.RawMessage) []json.RawMessage {
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) != nil {
		return []json.RawMessage{raw}
	}
	var out []json.RawMessage
	for _, e := range list {
		out = append(out, flatten(e)...)
	}
	return out
}

// langchainMessage reads one message in any of the shapes messagesOf lists.
func langchainMessage(raw json.RawMessage) (mlflowMessage, bool) {
	var wrap struct {
		LC     int             `json:"lc"`
		Kwargs json.RawMessage `json:"kwargs"`
		Type   string          `json:"type"`
		Data   json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &wrap) != nil {
		return mlflowMessage{}, false
	}
	switch {
	case wrap.LC > 0 && len(wrap.Kwargs) > 0:
		raw = wrap.Kwargs
	case wrap.Type != "" && len(wrap.Data) > 0 && strings.HasPrefix(strings.TrimSpace(string(wrap.Data)), "{"):
		raw = wrap.Data
	}
	var m mlflowMessage
	if json.Unmarshal(raw, &m) != nil || (m.Role == "" && langchainRoles[m.Type] == "") {
		return mlflowMessage{}, false
	}
	return m, true
}
