// Package spec parses the specs of EvalRun and OnlineEvalPolicy resources
// into their API messages, with the same rules as the CLI's run and policy
// files (python/evalsi/src/evalsi/runspec.py), so a file that `evalsi run -f`
// accepts is a resource kubectl can apply.
package spec

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

// Run parses and validates a run spec.
func Run(raw []byte) (*evalsiv1alpha1.RunSpec, error) {
	out := &evalsiv1alpha1.RunSpec{}
	if err := parse(raw, out); err != nil {
		return nil, err
	}
	return out, ValidateRun(out)
}

// Policy parses a policy spec; name, project and labels come from the
// resource, not the spec.
func Policy(raw []byte) (*evalsiv1alpha1.OnlineEvalPolicy, error) {
	out := &evalsiv1alpha1.OnlineEvalPolicy{}
	if err := parse(raw, out); err != nil {
		return nil, err
	}
	if out.GetName() != "" || out.GetProject() != "" || len(out.GetLabels()) > 0 {
		return nil, errors.New("spec cannot set name, project or labels; they come from metadata")
	}
	if len(out.GetStages()) == 0 {
		return nil, errors.New("spec.stages must list at least one stage")
	}
	for i, s := range out.GetStages() {
		if len(s.GetEvaluators()) == 0 {
			return nil, fmt.Errorf("spec.stages[%d] has no evaluators", i)
		}
	}
	return out, nil
}

func parse(raw []byte, m proto.Message) error {
	if len(raw) == 0 {
		return errors.New("spec is required")
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("spec: %w", err)
	}
	obj, ok := doc.(map[string]any)
	if !ok {
		return errors.New("spec must be an object")
	}
	normalized, err := json.Marshal(normalizeDurations(obj, m.ProtoReflect().Descriptor()))
	if err != nil {
		return err
	}
	if err := protojson.Unmarshal(normalized, m); err != nil {
		return fmt.Errorf("invalid spec: %w", err)
	}
	return nil
}

// ValidateRun applies the checks the CLI does before it sends a run.
func ValidateRun(s *evalsiv1alpha1.RunSpec) error {
	if len(s.GetEvaluators()) == 0 {
		return errors.New("spec.evaluators must list at least one evaluator")
	}
	if s.GetDataset().GetSource() == nil {
		return errors.New("spec.dataset needs one of inline, path, uri, traces or run")
	}
	if s.GetTrials() < 0 {
		return errors.New("spec.trials cannot be negative")
	}
	agentRun := s.GetHarness() != nil || s.GetTarget().GetAgent() != nil
	if s.GetTrials() > 1 && s.GetTarget() == nil && !agentRun {
		return errors.New("spec.trials above 1 needs a target; without one every trial is identical")
	}
	if s.GetTarget() != nil && !agentRun && s.GetTarget().GetModel() == "" {
		return errors.New("spec.target needs a model (or an agent)")
	}
	for _, g := range s.GetGates() {
		if g.GetMetric() == "" {
			return errors.New("every gate needs a metric")
		}
		if g.Min == nil && g.Max == nil {
			return fmt.Errorf("gate on %q needs min or max", g.GetMetric())
		}
	}
	return nil
}

var duration = regexp.MustCompile(`^(\d+(?:\.\d+)?)(ms|s|m|h)$`)

// normalizeDurations turns 250ms, 10m, 1.5h or a number of seconds into the
// seconds form protobuf JSON takes, wherever the schema has a Duration.
func normalizeDurations(obj map[string]any, md protoreflect.MessageDescriptor) map[string]any {
	out := make(map[string]any, len(obj))
	for key, value := range obj {
		fd := md.Fields().ByJSONName(key)
		if fd == nil {
			fd = md.Fields().ByName(protoreflect.Name(key))
		}
		if fd == nil || fd.Message() == nil || fd.IsMap() {
			out[key] = value
			continue
		}
		sub := fd.Message()
		convert := func(v any) any {
			if sub.FullName() == "google.protobuf.Duration" {
				return normalizeDuration(v)
			}
			if m, ok := v.(map[string]any); ok {
				return normalizeDurations(m, sub)
			}
			return v
		}
		if list, ok := value.([]any); ok && fd.IsList() {
			items := make([]any, len(list))
			for i, v := range list {
				items[i] = convert(v)
			}
			out[key] = items
		} else {
			out[key] = convert(value)
		}
	}
	return out
}

func normalizeDuration(v any) any {
	switch d := v.(type) {
	case float64:
		return strconv.FormatFloat(d, 'g', -1, 64) + "s"
	case string:
		m := duration.FindStringSubmatch(d)
		if m == nil {
			return d
		}
		n, _ := strconv.ParseFloat(m[1], 64)
		scale := map[string]float64{"ms": 0.001, "s": 1, "m": 60, "h": 3600}[m[2]]
		return strconv.FormatFloat(n*scale, 'g', -1, 64) + "s"
	}
	return v
}
