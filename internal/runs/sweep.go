package runs

import (
	"context"
	"fmt"
	"regexp"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/evaluation"
)

// maxSweepVariants bounds the runs one sweep creates.
const maxSweepVariants = 16

// Labels the server puts on the runs of a sweep.
const (
	SweepLabel   = "evalsi.sweep"
	VariantLabel = "evalsi.variant"
)

var variantName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,39}$`)

type sweepRun struct {
	variant string
	spec    *evalsiv1alpha1.RunSpec
}

// expandSweep turns a spec with a harness sweep into one spec per variant:
// each is the spec without the sweep, with the variant's fields replacing
// the same fields of spec.harness.builtin. A spec without a sweep expands to
// nothing.
func expandSweep(spec *evalsiv1alpha1.RunSpec) ([]sweepRun, error) {
	sweep := spec.GetSweep()
	if sweep == nil {
		return nil, nil
	}
	if spec.GetTarget().GetAgent() != nil || spec.GetHarness().GetExternal() != nil {
		return nil, invalid("spec.sweep varies the built-in agent's harness; it cannot be used with a bring-your-own agent or an external harness")
	}
	if n := len(sweep.GetVariants()); n == 0 || n > maxSweepVariants {
		return nil, invalid("spec.sweep.variants needs from 1 to %d variants", maxSweepVariants)
	}
	seen := map[string]bool{}
	dirs := map[string]string{}
	out := make([]sweepRun, 0, len(sweep.GetVariants()))
	for i, v := range sweep.GetVariants() {
		if !variantName.MatchString(v.GetName()) {
			return nil, invalid("spec.sweep.variants[%d].name must be letters, digits, dots, dashes and underscores (at most 40, starting with a letter or digit)", i)
		}
		if seen[v.GetName()] {
			return nil, invalid("spec.sweep.variants: %q appears twice", v.GetName())
		}
		seen[v.GetName()] = true
		if v.GetHarness() == nil {
			return nil, invalid("spec.sweep.variants[%d] (%s) sets no harness fields; the baseline is a variant with an explicit but empty harness", i, v.GetName())
		}
		expanded := proto.Clone(spec).(*evalsiv1alpha1.RunSpec)
		expanded.Sweep = nil
		if expanded.GetHarness() == nil {
			expanded.Harness = &evalsiv1alpha1.Harness{}
		}
		if expanded.GetHarness().GetBuiltin() == nil {
			expanded.Harness.Kind = &evalsiv1alpha1.Harness_Builtin{Builtin: &evalsiv1alpha1.BuiltinHarness{}}
		}
		base := expanded.GetHarness().GetBuiltin().ProtoReflect()
		proto.Clone(v.GetHarness()).ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, val protoreflect.Value) bool {
			base.Set(fd, val)
			return true
		})
		// Two variants recording into one directory would overwrite each other.
		if rec := expanded.GetHarness().GetBuiltin().GetRecording(); rec.GetMode() != "" && rec.GetMode() != "off" {
			if other, ok := dirs[rec.GetDir()]; ok {
				return nil, invalid("spec.sweep: variants %q and %q record into the same directory %q; give each variant its own recording.dir", other, v.GetName(), rec.GetDir())
			}
			dirs[rec.GetDir()] = v.GetName()
		}
		out = append(out, sweepRun{variant: v.GetName(), spec: expanded})
	}
	return out, nil
}

// validateSweep checks every expanded spec as a run of its own, naming the
// variant that fails.
func (m *Manager) validateSweep(ctx context.Context, project string, spec *evalsiv1alpha1.RunSpec) error {
	expanded, err := expandSweep(spec)
	if err != nil {
		return err
	}
	for _, e := range expanded {
		if _, err := m.validate(ctx, project, e.spec); err != nil {
			return fmt.Errorf("sweep variant %q: %w", e.variant, err)
		}
	}
	return nil
}

// createSweep stores and starts one run per variant over a single snapshot
// of the dataset. The caller has validated the spec.
func (m *Manager) createSweep(ctx context.Context, req *evalsiv1alpha1.CreateRunRequest, insts []evaluation.Instance) (*evalsiv1alpha1.CreateRunResponse, error) {
	expanded, err := expandSweep(req.GetSpec())
	if err != nil {
		return nil, err
	}
	project := projectOr(req.GetProject())
	if err := m.checkQuota(ctx, project, len(expanded)); err != nil {
		return nil, err
	}
	records, err := m.loadDataset(ctx, req.GetSpec().GetDataset(), project)
	if err != nil {
		return nil, err
	}
	m.admit.Lock()
	defer m.admit.Unlock()
	// Again, with the inserts: the dataset may have taken a while to load.
	if err := m.checkQuota(ctx, project, len(expanded)); err != nil {
		return nil, err
	}
	sweepID := newID()
	resp := &evalsiv1alpha1.CreateRunResponse{}
	for _, e := range expanded {
		labels := map[string]string{}
		for k, v := range req.GetLabels() {
			labels[k] = v
		}
		labels[SweepLabel], labels[VariantLabel] = sweepID, e.variant
		name := e.variant
		if req.GetName() != "" {
			name = req.GetName() + "/" + e.variant
		}
		run, err := m.storeRun(ctx, name, project, labels, e.spec, insts, records)
		if err != nil {
			return nil, fmt.Errorf("sweep variant %q: %w (variants before it were already started)", e.variant, err)
		}
		m.start(run)
		resp.SweepRuns = append(resp.SweepRuns, run)
	}
	resp.Run = resp.SweepRuns[0]
	return resp, nil
}
