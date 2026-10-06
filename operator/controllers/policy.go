package controllers

import (
	"context"
	"fmt"
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

// PolicyReconciler applies each OnlineEvalPolicy to the server, deletes it
// with the resource, and mirrors its counters.
type PolicyReconciler struct {
	client.Client
	API *API
	// How often counters are refreshed; default 30s.
	StatsInterval time.Duration
}

// +kubebuilder:rbac:groups=evals.si,resources=onlineevalpolicies,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=evals.si,resources=onlineevalpolicies/status;onlineevalpolicies/finalizers,verbs=get;update;patch

func (r *PolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	p := &v1.OnlineEvalPolicy{}
	if err := r.Get(ctx, req.NamespacedName, p); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// The API's policy names are global; the resource's name is used as is,
	// so `kubectl apply` and `evalsi policy apply` name a policy alike.
	name := p.Name
	if !p.DeletionTimestamp.IsZero() {
		if p.Status.Name != "" {
			_, err := r.API.Monitor.DeletePolicy(ctx, connect.NewRequest(&evalsiv1alpha1.DeletePolicyRequest{Name: p.Status.Name}))
			if err != nil && connect.CodeOf(err) != connect.CodeNotFound && !permanent(err) {
				return ctrl.Result{}, err
			}
		}
		if controllerutil.RemoveFinalizer(p, v1.Finalizer) {
			return ctrl.Result{}, r.Update(ctx, p)
		}
		return ctrl.Result{}, nil
	}
	if controllerutil.AddFinalizer(p, v1.Finalizer) {
		if err := r.Update(ctx, p); err != nil {
			return ctrl.Result{}, err
		}
	}
	synced := meta.IsStatusConditionTrue(p.Status.Conditions, "Synced")
	if !synced || p.Status.ObservedGeneration != p.Generation {
		return r.apply(ctx, p, name)
	}
	stats, err := r.API.Monitor.GetPolicyStats(ctx, connect.NewRequest(&evalsiv1alpha1.GetPolicyStatsRequest{Name: name}))
	if connect.CodeOf(err) == connect.CodeNotFound {
		return r.apply(ctx, p, name) // the server lost it (a new database): apply again
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	s := stats.Msg.GetStats()
	p.Status.TracesSeen, p.Status.TracesEvaluated = s.GetTracesSeen(), s.GetTracesEvaluated()
	p.Status.TracesPromoted, p.Status.EvaluationErrors = s.GetTracesPromoted(), s.GetEvaluationErrors()
	return ctrl.Result{RequeueAfter: r.interval()}, r.Status().Update(ctx, p)
}

func (r *PolicyReconciler) interval() time.Duration {
	if r.StatsInterval > 0 {
		return r.StatsInterval
	}
	return 30 * time.Second
}

func (r *PolicyReconciler) apply(ctx context.Context, p *v1.OnlineEvalPolicy, name string) (ctrl.Result, error) {
	fail := func(reason string, err error) (ctrl.Result, error) {
		p.Status.ObservedGeneration = p.Generation
		meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{Type: "Synced", Status: metav1.ConditionFalse, Reason: reason, Message: err.Error(), ObservedGeneration: p.Generation})
		return ctrl.Result{}, r.Status().Update(ctx, p)
	}
	policy, err := spec.Policy(p.Spec.Raw)
	if err != nil {
		return fail("Invalid", err)
	}
	project := Project(p)
	// Refuse to take over another project's policy of the same name.
	list, err := r.API.Monitor.ListPolicies(ctx, connect.NewRequest(&evalsiv1alpha1.ListPoliciesRequest{}))
	if err != nil {
		return ctrl.Result{}, err
	}
	for _, other := range list.Msg.GetPolicies() {
		if other.GetName() == name && other.GetProject() != project {
			return fail("Conflict", fmt.Errorf("policy %q already exists in project %q", name, other.GetProject()))
		}
	}
	policy.Name, policy.Project, policy.Labels = name, project, apiLabels(p)
	if _, err := r.API.Monitor.ApplyPolicy(ctx, connect.NewRequest(&evalsiv1alpha1.ApplyPolicyRequest{Policy: policy})); err != nil {
		if permanent(err) {
			return fail("Rejected", err)
		}
		return ctrl.Result{}, err
	}
	p.Status.Name, p.Status.ObservedGeneration = name, p.Generation
	meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{Type: "Synced", Status: metav1.ConditionTrue, Reason: "Applied", Message: "applied to project " + project, ObservedGeneration: p.Generation})
	return ctrl.Result{RequeueAfter: r.interval()}, r.Status().Update(ctx, p)
}

func (r *PolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&v1.OnlineEvalPolicy{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).Named("onlineevalpolicy").Complete(r)
}
