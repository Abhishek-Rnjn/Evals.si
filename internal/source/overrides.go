package source

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types/ref"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/ingest"
)

// Overrides (decision 0016, item 20) are CEL expressions that set record
// fields the conventions did not. They see what policy selectors see
// (service, name, attributes of the root span, resource attributes, labels)
// plus the record's input and output as text, and run after the conventions,
// so they only add to or replace the fields they name.
var overrideEnv = func() *cel.Env {
	env, err := cel.NewEnv(
		cel.Variable("service", cel.StringType),
		cel.Variable("name", cel.StringType),
		cel.Variable("attributes", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("resource", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("labels", cel.MapType(cel.StringType, cel.StringType)),
		cel.Variable("input", cel.StringType),
		cel.Variable("output", cel.StringType),
	)
	if err != nil {
		panic(err)
	}
	return env
}()

type override struct {
	field, key string
	expr       string
	prog       cel.Program
}

// CompileOverrides checks a source's overrides and returns what applies them
// to a record (nil when there are none).
func CompileOverrides(m map[string]string) (func(*evalsiv1alpha1.Record, *ingest.TraceInfo), error) {
	if len(m) == 0 {
		return nil, nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var ovs []override
	for _, k := range keys {
		field, key, _ := strings.Cut(k, ".")
		switch field {
		case "input", "output", "reference", "context":
			if key != "" {
				return nil, fmt.Errorf("source.overrides[%q]: %s takes no key", k, field)
			}
		case "metadata", "labels":
			if key == "" {
				return nil, fmt.Errorf("source.overrides[%q]: name the key, as %s.<key>", k, field)
			}
		default:
			return nil, fmt.Errorf("source.overrides[%q]: can set input, output, reference, context, metadata.<key> or labels.<key>", k)
		}
		ast, issues := overrideEnv.Compile(m[k])
		if issues != nil && issues.Err() != nil {
			return nil, fmt.Errorf("source.overrides[%q]: %w", k, issues.Err())
		}
		prog, err := overrideEnv.Program(ast, cel.CostLimit(100000))
		if err != nil {
			return nil, fmt.Errorf("source.overrides[%q]: %w", k, err)
		}
		ovs = append(ovs, override{field: field, key: key, expr: m[k], prog: prog})
	}
	return func(rec *evalsiv1alpha1.Record, info *ingest.TraceInfo) {
		vars := map[string]any{
			"service": info.Service, "name": info.Name,
			"attributes": map[string]any(info.Attributes), "resource": map[string]any(info.Resource),
			"labels": labelsOrEmpty(info.Labels), "input": contentText(rec.GetInput()), "output": contentText(rec.GetOutput()),
		}
		for _, o := range ovs {
			out, _, err := o.prog.Eval(vars)
			if err != nil {
				continue // a missing key or a type error leaves the field as the conventions made it
			}
			o.apply(rec, info, native(out))
		}
	}, nil
}

func (o override) apply(rec *evalsiv1alpha1.Record, info *ingest.TraceInfo, v any) {
	if v == nil {
		return
	}
	switch o.field {
	case "input":
		rec.Input = toContent(v)
	case "output":
		rec.Output = toContent(v)
	case "reference":
		rec.Reference = toContent(v)
	case "context":
		if list, ok := v.([]any); ok {
			rec.Context = nil
			for _, item := range list {
				if c := toContent(item); c != nil {
					rec.Context = append(rec.Context, c)
				}
			}
		} else if c := toContent(v); c != nil {
			rec.Context = []*evalsiv1alpha1.Content{c}
		}
	case "metadata":
		if val, err := structpb.NewValue(v); err == nil {
			if rec.Metadata == nil {
				rec.Metadata = map[string]*structpb.Value{}
			}
			rec.Metadata[o.key] = val
		}
	case "labels":
		labels := map[string]string{}
		for k, x := range info.Labels {
			labels[k] = x
		}
		labels[o.key] = fmt.Sprint(v)
		info.Labels = labels
	}
}

// native is a CEL result as plain Go: through a protobuf Value, which CEL
// converts its maps, lists and scalars to.
func native(v ref.Val) any {
	if _, null := v.Value().(structpb.NullValue); null {
		return nil
	}
	out, err := v.ConvertToNative(reflect.TypeOf(&structpb.Value{}))
	if err != nil {
		return v.Value()
	}
	pv, ok := out.(*structpb.Value)
	if !ok {
		return v.Value()
	}
	return pv.AsInterface()
}

func toContent(v any) *evalsiv1alpha1.Content {
	if s, ok := v.(string); ok {
		return &evalsiv1alpha1.Content{Kind: &evalsiv1alpha1.Content_Text{Text: s}}
	}
	val, err := structpb.NewValue(normalizeJSON(v))
	if err != nil {
		return &evalsiv1alpha1.Content{Kind: &evalsiv1alpha1.Content_Text{Text: fmt.Sprint(v)}}
	}
	return &evalsiv1alpha1.Content{Kind: &evalsiv1alpha1.Content_Json{Json: val}}
}

// normalizeJSON turns what CEL hands back (structpb values, maps keyed by
// any) into plain JSON values.
func normalizeJSON(v any) any {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	var out any
	if json.Unmarshal(raw, &out) != nil {
		return fmt.Sprint(v)
	}
	return out
}

// contentText is a content as text: the text itself, or its JSON.
func contentText(c *evalsiv1alpha1.Content) string {
	if c == nil {
		return ""
	}
	if t, ok := c.GetKind().(*evalsiv1alpha1.Content_Text); ok {
		return t.Text
	}
	raw, err := protojson.Marshal(c)
	if err != nil {
		return ""
	}
	return string(raw)
}

func labelsOrEmpty(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}
