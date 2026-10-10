package controllers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	v1 "github.com/abhishek-rnjn/evals.si/operator/api/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/operator/spec"
)

// Phases of an EvalRun.
const (
	PhasePending   = "Pending"
	PhaseRunning   = "Running"
	PhaseSucceeded = "Succeeded"
	PhaseFailed    = "Failed"
	PhaseError     = "Error"
	PhaseCancelled = "Cancelled"
	PhaseInvalid   = "Invalid"
)

var phases = map[evalsiv1alpha1.RunStatus]string{
	evalsiv1alpha1.RunStatus_RUN_STATUS_PENDING:   PhasePending,
	evalsiv1alpha1.RunStatus_RUN_STATUS_RUNNING:   PhaseRunning,
	evalsiv1alpha1.RunStatus_RUN_STATUS_SUCCEEDED: PhaseSucceeded,
	evalsiv1alpha1.RunStatus_RUN_STATUS_FAILED:    PhaseFailed,
	evalsiv1alpha1.RunStatus_RUN_STATUS_ERROR:     PhaseError,
	evalsiv1alpha1.RunStatus_RUN_STATUS_CANCELLED: PhaseCancelled,
}

func finished(phase string) bool {
	switch phase {
	case PhaseSucceeded, PhaseFailed, PhaseError, PhaseCancelled, PhaseInvalid:
		return true
	}
	return false
}

// EvalRunReconciler creates each EvalRun's run and mirrors its state.
type EvalRunReconciler struct {
	client.Client
	API *API
	// How often a running run's state is polled; default 5s.
	PollInterval time.Duration
}

// Project is the evalsi project of a namespaced resource: its
// evals.si/project label, else its namespace.
func Project(obj metav1.Object) string { return v1.ProjectOf(obj) }

// apiLabels are the resource's labels that travel to the API (for access
// rules), without Kubernetes' own prefixed ones.
func apiLabels(obj metav1.Object) map[string]string { return v1.APILabels(obj) }

// appliedMetadata digests what an applied resource's API object takes from
// its metadata (the project and API labels), so a label-only edit, which
// leaves the generation alone, is seen and applied.
func appliedMetadata(obj metav1.Object) string {
	labels := apiLabels(obj)
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	fmt.Fprintf(h, "%q", Project(obj))
	for _, k := range keys {
		fmt.Fprintf(h, "\x00%q=%q", k, labels[k])
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// metadataChanged passes events where the generation or the labels changed.
var metadataChanged = predicate.Or(predicate.GenerationChangedPredicate{}, predicate.LabelChangedPredicate{})

// +kubebuilder:rbac:groups=evals.si,resources=evalruns,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=evals.si,resources=evalruns/status;evalruns/finalizers,verbs=get;update;patch

func (r *EvalRunReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	run := &v1.EvalRun{}
	if err := r.Get(ctx, req.NamespacedName, run); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !run.DeletionTimestamp.IsZero() {
		// Deleting the resource cancels the run; its results stay.
		if run.Status.RunID != "" && !finished(run.Status.Phase) {
			_, err := r.API.Runs.CancelRun(ctx, connect.NewRequest(&evalsiv1alpha1.CancelRunRequest{Id: run.Status.RunID}))
			if err != nil && connect.CodeOf(err) != connect.CodeNotFound && !permanent(err) {
				return ctrl.Result{}, err
			}
		}
		if controllerutil.RemoveFinalizer(run, v1.Finalizer) {
			return ctrl.Result{}, r.Update(ctx, run)
		}
		return ctrl.Result{}, nil
	}
	if run.Status.RunID == "" && run.Status.Phase == PhaseInvalid {
		return ctrl.Result{}, nil // the spec is immutable: nothing will change
	}
	if run.Status.RunID == "" && controllerutil.AddFinalizer(run, v1.Finalizer) {
		if err := r.Update(ctx, run); err != nil {
			return ctrl.Result{}, err
		}
	}

	if run.Status.RunID == "" {
		return r.create(ctx, run)
	}
	got, err := r.API.Runs.GetRun(ctx, connect.NewRequest(&evalsiv1alpha1.GetRunRequest{Id: run.Status.RunID}))
	if err != nil {
		if connect.CodeOf(err) == connect.CodeNotFound {
			return ctrl.Result{}, r.setStatus(ctx, run, func(s *v1.EvalRunStatus) {
				s.Phase, s.Message = PhaseError, "the run is gone from the server"
			})
		}
		return ctrl.Result{}, err
	}
	m := got.Msg.GetRun()
	if err := r.setStatus(ctx, run, func(s *v1.EvalRunStatus) { mirror(s, m) }); err != nil {
		return ctrl.Result{}, err
	}
	if finished(phases[m.GetStatus()]) {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: r.poll()}, nil
}

func (r *EvalRunReconciler) poll() time.Duration {
	if r.PollInterval > 0 {
		return r.PollInterval
	}
	return 5 * time.Second
}

func (r *EvalRunReconciler) create(ctx context.Context, run *v1.EvalRun) (ctrl.Result, error) {
	s, err := spec.Run(run.Spec.Raw)
	if err != nil {
		return ctrl.Result{}, r.invalid(ctx, run, err)
	}
	var created *connect.Response[evalsiv1alpha1.CreateRunResponse]
	err = r.API.withProject(ctx, Project(run), func() (err error) {
		created, err = r.API.Runs.CreateRun(ctx, connect.NewRequest(&evalsiv1alpha1.CreateRunRequest{
			Name: run.Name, Project: Project(run), Spec: s, Labels: apiLabels(run),
		}))
		return err
	})
	if err != nil {
		if permanent(err) {
			return ctrl.Result{}, r.invalid(ctx, run, err)
		}
		return ctrl.Result{}, err
	}
	m := created.Msg.GetRun()
	log.FromContext(ctx).Info("created run", "run", m.GetId(), "project", m.GetProject())
	err = r.setStatus(ctx, run, func(s *v1.EvalRunStatus) {
		s.RunID, s.Project = m.GetId(), m.GetProject()
		meta.SetStatusCondition(&s.Conditions, metav1.Condition{Type: "Accepted", Status: metav1.ConditionTrue, Reason: "Created", Message: "run " + m.GetId()})
		mirror(s, m)
	})
	if err != nil {
		// The run exists but is not recorded; cancel it rather than leave
		// it to run twice when this is retried.
		_, _ = r.API.Runs.CancelRun(ctx, connect.NewRequest(&evalsiv1alpha1.CancelRunRequest{Id: m.GetId()}))
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: r.poll()}, nil
}

func (r *EvalRunReconciler) invalid(ctx context.Context, run *v1.EvalRun, err error) error {
	return r.setStatus(ctx, run, func(s *v1.EvalRunStatus) {
		s.Phase, s.Message = PhaseInvalid, err.Error()
		meta.SetStatusCondition(&s.Conditions, metav1.Condition{Type: "Accepted", Status: metav1.ConditionFalse, Reason: "Invalid", Message: err.Error()})
	})
}

// setStatus applies change to the latest version of the resource's status.
func (r *EvalRunReconciler) setStatus(ctx context.Context, run *v1.EvalRun, change func(*v1.EvalRunStatus)) error {
	first := true
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if !first {
			if err := r.Get(ctx, client.ObjectKeyFromObject(run), run); err != nil {
				return err
			}
		}
		first = false
		change(&run.Status)
		return r.Status().Update(ctx, run)
	})
}

func mirror(s *v1.EvalRunStatus, m *evalsiv1alpha1.Run) {
	s.Phase = phases[m.GetStatus()]
	s.Message = m.GetError()
	if p := m.GetProgress(); p != nil {
		s.Progress = &v1.Progress{Done: p.GetDone(), Total: p.GetTotal()}
	}
	if t := m.GetStartedAt(); t != nil {
		mt := metav1.NewTime(t.AsTime())
		s.StartedAt = &mt
	}
	if t := m.GetFinishedAt(); t != nil {
		mt := metav1.NewTime(t.AsTime())
		s.FinishedAt = &mt
	}
	s.Summaries = nil
	for _, x := range m.GetSummaries() {
		out := v1.MetricSummary{Metric: x.GetMetric(), N: x.GetN(), Skipped: x.GetSkipped(), Errors: x.GetErrors()}
		if x.Mean != nil {
			out.Mean = num(x.GetMean())
		}
		if ci := x.GetCi(); ci != nil {
			out.CILow, out.CIHigh = num(ci.GetLow()), num(ci.GetHigh())
		}
		s.Summaries = append(s.Summaries, out)
	}
	s.Gates = nil
	for _, g := range m.GetGates() {
		s.Gates = append(s.Gates, v1.GateResult{Metric: g.GetGate().GetMetric(), Passed: g.GetPassed(), Reason: g.GetReason()})
	}
	if finished(s.Phase) {
		status := metav1.ConditionTrue
		if s.Phase != PhaseSucceeded {
			status = metav1.ConditionFalse
		}
		meta.SetStatusCondition(&s.Conditions, metav1.Condition{Type: "Succeeded", Status: status, Reason: s.Phase, Message: strings.TrimSuffix(s.Phase+": "+s.Message, ": ")})
	}
}

func num(f float64) string { return strconv.FormatFloat(f, 'g', 6, 64) }

func (r *EvalRunReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&v1.EvalRun{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).Named("evalrun").Complete(r)
}
