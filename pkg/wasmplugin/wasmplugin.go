// Package wasmplugin writes Evals.si evaluators that compile to WebAssembly
// (GOOS=wasip1 GOARCH=wasm) and run sandboxed inside evalsid.
//
//	func main() {
//		wasmplugin.Main(wasmplugin.Plugin{
//			Evaluators: map[string]wasmplugin.Evaluator{
//				"acme/json-valid": func(r wasmplugin.Record, _ wasmplugin.Params) wasmplugin.Result {
//					var v any
//					if err := json.Unmarshal([]byte(r.Output.String()), &v); err != nil {
//						return wasmplugin.Scores(wasmplugin.Fail(err.Error()))
//					}
//					return wasmplugin.Scores(wasmplugin.Pass())
//				},
//			},
//		})
//	}
//
// It implements evalsi wasm ABI 1 (see internal/wasmeval): JSON on stdin,
// JSON on stdout, nothing else. Any language that targets WASI preview 1
// can implement the same ABI directly.
package wasmplugin

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// Message is one chat message of a Content.
type Message struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
	Name    string `json:"name,omitempty"`
}

// Content is a record field: text, chat messages or JSON.
type Content struct {
	Text     string `json:"text,omitempty"`
	JSON     any    `json:"json,omitempty"`
	Messages *struct {
		Messages []Message `json:"messages,omitempty"`
	} `json:"messages,omitempty"`
}

// String is the content as text: the text, the messages one per line, or
// the JSON encoded.
func (c *Content) String() string {
	switch {
	case c == nil:
		return ""
	case c.Text != "":
		return c.Text
	case c.Messages != nil:
		var b strings.Builder
		for i, m := range c.Messages.Messages {
			if i > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(m.Content)
		}
		return b.String()
	case c.JSON != nil:
		out, _ := json.Marshal(c.JSON)
		return string(out)
	}
	return ""
}

// Record is what an evaluator scores (the fields Wasm evaluators use).
type Record struct {
	ID        string         `json:"id,omitempty"`
	Input     *Content       `json:"input,omitempty"`
	Output    *Content       `json:"output,omitempty"`
	Reference *Content       `json:"reference,omitempty"`
	Context   []*Content     `json:"context,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// Params are the evaluator's params from the request (defaults are the
// evaluator's job).
type Params map[string]any

// Number returns a numeric param, or def.
func (p Params) Number(name string, def float64) float64 {
	if v, ok := p[name].(float64); ok {
		return v
	}
	return def
}

// String returns a string param, or def.
func (p Params) String(name, def string) string {
	if v, ok := p[name].(string); ok {
		return v
	}
	return def
}

// Score is one metric value. Set exactly one of Number, Passed and Label.
type Score struct {
	Name        string   `json:"name,omitempty"`
	Number      *float64 `json:"number,omitempty"`
	Passed      *bool    `json:"passed,omitempty"`
	Label       string   `json:"label,omitempty"`
	Explanation string   `json:"explanation,omitempty"`
}

// Pass is a passing pass/fail score.
func Pass() Score { t := true; return Score{Passed: &t} }

// Fail is a failing pass/fail score with an explanation.
func Fail(explanation string) Score { f := false; return Score{Passed: &f, Explanation: explanation} }

// Value is a numeric score.
func Value(v float64) Score { return Score{Number: &v} }

// Named sets the score's metric name (needed when an evaluator emits several).
func (s Score) Named(name string) Score { s.Name = name; return s }

// Result is one record's outcome.
type Result struct {
	Outcome string  `json:"outcome,omitempty"`
	Scores  []Score `json:"scores,omitempty"`
	Reason  string  `json:"reason,omitempty"`
}

// Scores is a scored result.
func Scores(s ...Score) Result { return Result{Outcome: "OUTCOME_SCORED", Scores: s} }

// Skip says the record lacks what the evaluator needs.
func Skip(reason string) Result { return Result{Outcome: "OUTCOME_SKIPPED", Reason: reason} }

// Error says the evaluator failed on this record.
func Error(reason string) Result { return Result{Outcome: "OUTCOME_ERROR", Reason: reason} }

// Evaluator scores one record.
type Evaluator func(Record, Params) Result

// Reducer scores a whole dataset (scope: dataset in the manifest).
type Reducer func([]Record, Params) []Score

// Plugin is a module's evaluators, by the names in its manifest.
type Plugin struct {
	Evaluators map[string]Evaluator
	Reducers   map[string]Reducer
}

type request struct {
	ABI       int      `json:"abi"`
	Evaluator string   `json:"evaluator"`
	Params    Params   `json:"params"`
	Records   []Record `json:"records"`
}

// Main runs the plugin: it reads the request, answers, and exits.
func Main(p Plugin) {
	if err := Serve(p, os.Args, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// Serve answers one request; Main with explicit arguments and streams,
// for testing a plugin natively.
func Serve(p Plugin, args []string, in io.Reader, out io.Writer) error {
	verb := ""
	if len(args) > 1 {
		verb = args[1]
	}
	var req request
	if err := json.NewDecoder(in).Decode(&req); err != nil {
		return fmt.Errorf("reading the request: %w", err)
	}
	if req.ABI != 1 {
		return fmt.Errorf("unsupported ABI %d (this plugin speaks 1)", req.ABI)
	}
	if req.Params == nil {
		req.Params = Params{}
	}
	enc := json.NewEncoder(out)
	switch verb {
	case "evaluate":
		fn, ok := p.Evaluators[req.Evaluator]
		if !ok {
			return fmt.Errorf("no evaluator %s in this plugin", req.Evaluator)
		}
		results := make([]Result, len(req.Records))
		for i, r := range req.Records {
			results[i] = safely(fn, r, req.Params)
		}
		return enc.Encode(map[string]any{"results": results})
	case "reduce":
		fn, ok := p.Reducers[req.Evaluator]
		if !ok {
			return fmt.Errorf("no dataset evaluator %s in this plugin", req.Evaluator)
		}
		return enc.Encode(map[string]any{"scores": fn(req.Records, req.Params)})
	default:
		return fmt.Errorf("unknown verb %q (evaluate or reduce)", verb)
	}
}

// safely turns a panic on one record into that record's error.
func safely(fn Evaluator, r Record, p Params) (res Result) {
	defer func() {
		if v := recover(); v != nil {
			res = Error(fmt.Sprint("panic: ", v))
		}
	}()
	return fn(r, p)
}
