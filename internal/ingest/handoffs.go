package ingest

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"google.golang.org/protobuf/types/known/structpb"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

// invokedAgent is the agent an invoke_agent span (GenAI conventions) runs,
// or "" for any other span.
func invokedAgent(s *evalsiv1alpha1.Step) string {
	attrs := s.GetAttributes()
	if attrs["gen_ai.operation.name"].GetStringValue() != "invoke_agent" {
		return ""
	}
	if name := attrs["gen_ai.agent.name"].GetStringValue(); name != "" {
		return name
	}
	// The convention names the span "invoke_agent {gen_ai.agent.name}".
	return strings.TrimPrefix(s.GetName(), "invoke_agent ")
}

// linkHandoffs adds a handoff step wherever one agent invokes another: an
// invoke_agent span whose nearest invoke_agent ancestor runs a different
// agent. The handoff sits between the two (the ancestor's subtree is its
// source parent, the sub-agent its target child), the shape
// builtin/handoff-correctness checks. Only the GenAI invoke_agent convention
// counts: other conventions mark framework internals (graph nodes, chains)
// as agents, and those are not transfers.
func linkHandoffs(traceID string, steps []*evalsiv1alpha1.Step) []*evalsiv1alpha1.Step {
	byID := make(map[string]*evalsiv1alpha1.Step, len(steps))
	for _, s := range steps {
		byID[s.GetSpanId()] = s
	}
	out := make([]*evalsiv1alpha1.Step, 0, len(steps))
	for _, s := range steps {
		target := invokedAgent(s)
		if target == "" {
			out = append(out, s)
			continue
		}
		source := ""
		seen := map[string]bool{}
		for p := byID[s.GetParentSpanId()]; p != nil && !seen[p.GetSpanId()]; p = byID[p.GetParentSpanId()] {
			seen[p.GetSpanId()] = true
			if source = invokedAgent(p); source != "" {
				break
			}
		}
		if source == "" || source == target {
			out = append(out, s)
			continue
		}
		sum := sha256.Sum256([]byte(traceID + "/" + s.GetSpanId() + "/handoff"))
		handoff := &evalsiv1alpha1.Step{
			SpanId:       hex.EncodeToString(sum[:8]),
			ParentSpanId: s.GetParentSpanId(),
			Type:         evalsiv1alpha1.StepType_STEP_TYPE_HANDOFF,
			Name:         target,
			StartTime:    s.GetStartTime(),
			EndTime:      s.GetStartTime(),
			Attributes: map[string]*structpb.Value{
				"evalsi.handoff.from": structpb.NewStringValue(source),
				"evalsi.handoff.to":   structpb.NewStringValue(target),
			},
		}
		s.ParentSpanId = handoff.GetSpanId()
		out = append(out, handoff, s)
	}
	return out
}
