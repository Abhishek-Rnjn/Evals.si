package controllers

import (
	"context"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	v1 "github.com/abhishek-rnjn/evals.si/operator/api/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/operator/spec"
)

// TraceSourceReconciler applies each TraceSource to the server, deletes it
// with the resource, and mirrors its status (phase, watermark, counters).
type TraceSourceReconciler struct {
	client.Client
	API *API
	// How often status is refreshed; default 30s.
	StatsInterval time.Duration
}

// +kubebuilder:rbac:groups=evals.si,resources=tracesources,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=evals.si,resources=tracesources/status;tracesources/finalizers,verbs=get;update;patch

func (r *TraceSourceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	s := &v1.TraceSource{}
	if err := r.Get(ctx, req.NamespacedName, s); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	project := Project(s)
	if !s.DeletionTimestamp.IsZero() {
		if s.Status.Name != "" {
			_, err := r.API.Sources.DeleteSource(ctx, connect.NewRequest(&evalsiv1alpha1.DeleteSourceRequest{Project: project, Name: s.Status.Name}))
			if err != nil && connect.CodeOf(err) != connect.CodeNotFound && !permanent(err) {
				return ctrl.Result{}, err
			}
		}
		if controllerutil.RemoveFinalizer(s, v1.Finalizer) {
			return ctrl.Result{}, r.Update(ctx, s)
		}
		return ctrl.Result{}, nil
	}
	if controllerutil.AddFinalizer(s, v1.Finalizer) {
		if err := r.Update(ctx, s); err != nil {
			return ctrl.Result{}, err
		}
	}
	synced := meta.IsStatusConditionTrue(s.Status.Conditions, "Synced")
	if !synced || s.Status.ObservedGeneration != s.Generation {
		return r.apply(ctx, s, project)
	}
	got, err := r.API.Sources.GetSource(ctx, connect.NewRequest(&evalsiv1alpha1.GetSourceRequest{Project: project, Name: s.Name}))
	if connect.CodeOf(err) == connect.CodeNotFound {
		return r.apply(ctx, s, project) // the server lost it (a new database): apply again
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	mirrorSource(&s.Status, got.Msg.GetSource().GetStatus())
	return ctrl.Result{RequeueAfter: r.interval()}, r.Status().Update(ctx, s)
}

func (r *TraceSourceReconciler) interval() time.Duration {
	if r.StatsInterval > 0 {
		return r.StatsInterval
	}
	return 30 * time.Second
}

func (r *TraceSourceReconciler) apply(ctx context.Context, s *v1.TraceSource, project string) (ctrl.Result, error) {
	fail := func(reason string, err error) (ctrl.Result, error) {
		s.Status.ObservedGeneration = s.Generation
		meta.SetStatusCondition(&s.Status.Conditions, metav1.Condition{Type: "Synced", Status: metav1.ConditionFalse, Reason: reason, Message: err.Error(), ObservedGeneration: s.Generation})
		return ctrl.Result{}, r.Status().Update(ctx, s)
	}
	src, err := spec.Source(s.Spec.Raw)
	if err != nil {
		return fail("Invalid", err)
	}
	src.Name, src.Project, src.Labels = s.Name, project, apiLabels(s)
	var applied *evalsiv1alpha1.TraceSource
	err = r.API.withProject(ctx, project, func() error {
		resp, err := r.API.Sources.ApplySource(ctx, connect.NewRequest(&evalsiv1alpha1.ApplySourceRequest{Source: src}))
		if err == nil {
			applied = resp.Msg.GetSource()
		}
		return err
	})
	if err != nil {
		if permanent(err) {
			return fail("Rejected", err)
		}
		return ctrl.Result{}, err
	}
	s.Status.Name, s.Status.ObservedGeneration = s.Name, s.Generation
	mirrorSource(&s.Status, applied.GetStatus())
	meta.SetStatusCondition(&s.Status.Conditions, metav1.Condition{Type: "Synced", Status: metav1.ConditionTrue, Reason: "Applied", Message: "applied to project " + project, ObservedGeneration: s.Generation})
	return ctrl.Result{RequeueAfter: r.interval()}, r.Status().Update(ctx, s)
}

// mirrorSource copies the API's status onto the resource.
func mirrorSource(dst *v1.TraceSourceStatus, src *evalsiv1alpha1.SourceStatus) {
	phase := strings.TrimPrefix(src.GetPhase().String(), "SOURCE_PHASE_")
	if src.GetPhase() == evalsiv1alpha1.SourcePhase_SOURCE_PHASE_UNSPECIFIED {
		phase = "Pending"
	} else {
		phase = phase[:1] + strings.ToLower(phase[1:])
	}
	dst.Phase = phase
	dst.Watermark, dst.LastWriteBack = nil, nil
	if src.GetWatermark().IsValid() {
		t := metav1.NewTime(src.GetWatermark().AsTime())
		dst.Watermark = &t
	}
	if src.GetLastWriteBackAt().IsValid() {
		t := metav1.NewTime(src.GetLastWriteBackAt().AsTime())
		dst.LastWriteBack = &t
	}
	dst.LagSeconds = ""
	if src.GetLastPullAt().IsValid() {
		dst.LagSeconds = strconv.FormatFloat(src.GetLagSeconds(), 'f', 0, 64)
	}
	dst.Pulled, dst.Scored, dst.Deferred, dst.LastError = src.GetPulled(), src.GetScored(), src.GetDeferred(), src.GetLastError()
}

func (r *TraceSourceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&v1.TraceSource{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).Named("tracesource").Complete(r)
}
