package controllers

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "github.com/abhishek-rnjn/evals.si/operator/api/v1alpha1"
)

func TestEvaluatorRemovesScaledObjectWhenAutoscalingRemoved(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1.AddToScheme(scheme)
	rm := meta.NewDefaultRESTMapper(nil)
	rm.Add(scaledObjectGVK, meta.RESTScopeNamespace)
	ev := &v1.Evaluator{ObjectMeta: metav1.ObjectMeta{Name: "e", Namespace: "ns"},
		Spec: v1.EvaluatorSpec{Image: "i", Autoscaling: &v1.Autoscaling{MinReplicas: 1, MaxReplicas: 5}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(rm).WithObjects(ev).WithStatusSubresource(ev).Build()
	r := &EvaluatorReconciler{Client: c, NATSMonitoringEndpoint: "n:8222"}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "e"}}
	ctx := context.Background()
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	so := &unstructured.Unstructured{}
	so.SetGroupVersionKind(scaledObjectGVK)
	if err := c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "evalsi-evaluator-e"}, so); err != nil {
		t.Fatal("no scaledobject", err)
	}
	_ = c.Get(ctx, req.NamespacedName, ev)
	ev.Spec.Autoscaling = nil
	two := int32(2)
	ev.Spec.Replicas = &two
	if err := c.Update(ctx, ev); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "evalsi-evaluator-e"}, so); err == nil {
		d := &appsv1.Deployment{}
		_ = c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "evalsi-evaluator-e"}, d)
		t.Errorf("ScaledObject still exists after autoscaling removed (deployment replicas %d)", *d.Spec.Replicas)
	}
}

func TestEvaluatorConfigHashRollsPods(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1.AddToScheme(scheme)
	ev := &v1.Evaluator{ObjectMeta: metav1.ObjectMeta{Name: "e", Namespace: "ns"}, Spec: v1.EvaluatorSpec{Image: "i", ConfigMap: "cfg"}}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cfg", Namespace: "ns"}, Data: map[string]string{"evalsi.yaml": "a: 1"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ev, cm).WithStatusSubresource(ev).Build()
	r := &EvaluatorReconciler{Client: c}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "e"}}
	ctx := context.Background()
	annotation := func() string {
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatal(err)
		}
		d := &appsv1.Deployment{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "evalsi-evaluator-e"}, d); err != nil {
			t.Fatal(err)
		}
		return d.Spec.Template.Annotations["evals.si/config-hash"]
	}
	before := annotation()
	if before == "" {
		t.Fatal("no config-hash annotation")
	}
	cm.Data["evalsi.yaml"] = "a: 2"
	if err := c.Update(ctx, cm); err != nil {
		t.Fatal(err)
	}
	if after := annotation(); after == before {
		t.Errorf("ConfigMap edit left the pod template unchanged")
	}
}
