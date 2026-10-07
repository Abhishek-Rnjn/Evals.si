// Command text-checks is an example Wasm evaluator plugin: deterministic
// text checks that run sandboxed inside evalsid.
//
//	GOOS=wasip1 GOARCH=wasm go build -o text-checks.wasm .
//	evalsid wasm pin evalsi-plugin.yaml
//	evalsid wasm check evalsi-plugin.yaml
package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/abhishek-rnjn/evals.si/pkg/wasmplugin"
)

func main() {
	wasmplugin.Main(plugin)
}

var plugin = wasmplugin.Plugin{
	Evaluators: map[string]wasmplugin.Evaluator{
		"example/json-valid": jsonValid,
		"example/word-limit": wordLimit,
	},
	Reducers: map[string]wasmplugin.Reducer{
		"example/distinct-outputs": distinctOutputs,
	},
}

// jsonValid passes when the output is a JSON object with the required keys.
func jsonValid(r wasmplugin.Record, p wasmplugin.Params) wasmplugin.Result {
	if r.Output == nil {
		return wasmplugin.Skip("no output")
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(r.Output.String())), &obj); err != nil {
		return wasmplugin.Scores(wasmplugin.Fail("not a JSON object: " + err.Error()))
	}
	if keys, ok := p["required_keys"].([]any); ok {
		for _, k := range keys {
			if _, ok := obj[fmt.Sprint(k)]; !ok {
				return wasmplugin.Scores(wasmplugin.Fail(fmt.Sprintf("missing key %q", k)))
			}
		}
	}
	return wasmplugin.Scores(wasmplugin.Pass())
}

// wordLimit passes when the output has at most max_words words, and reports
// the count.
func wordLimit(r wasmplugin.Record, p wasmplugin.Params) wasmplugin.Result {
	n := len(strings.Fields(r.Output.String()))
	limit := int(p.Number("max_words", 100))
	pass := wasmplugin.Pass()
	if n > limit {
		pass = wasmplugin.Fail(fmt.Sprintf("%d words; the limit is %d", n, limit))
	}
	return wasmplugin.Scores(pass.Named("word-limit"), wasmplugin.Value(float64(n)).Named("words"))
}

// distinctOutputs is the share of records whose output is unique.
func distinctOutputs(records []wasmplugin.Record, _ wasmplugin.Params) []wasmplugin.Score {
	if len(records) == 0 {
		return nil
	}
	seen := map[string]bool{}
	for _, r := range records {
		seen[strings.TrimSpace(r.Output.String())] = true
	}
	return []wasmplugin.Score{wasmplugin.Value(float64(len(seen)) / float64(len(records)))}
}
