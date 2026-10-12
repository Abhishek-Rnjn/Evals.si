package controllers

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "github.com/abhishek-rnjn/evals.si/operator/api/v1alpha1"
)

func checkpointReconciler(t *testing.T, objs ...client.Object) (*CheckpointServerReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithStatusSubresource(&v1.CheckpointServer{}).Build()
	return &CheckpointServerReconciler{Client: c, NamePrefix: "evalsi"}, c
}

func reconcile(t *testing.T, r *CheckpointServerReconciler, namespace, name string) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: name}}); err != nil {
		t.Fatal(err)
	}
}

// A CheckpointServer becomes a vLLM Deployment and Service, and its status
// says where to send requests and whether the model is loaded.
func TestCheckpointServerRunsVLLM(t *testing.T) {
	cs := &v1.CheckpointServer{
		ObjectMeta: metav1.ObjectMeta{Name: "step-200", Namespace: "team-a"},
		Spec: v1.CheckpointServerSpec{
			Image: "vllm/vllm-openai:v0.11.0", Model: "/checkpoints/run-1/step-200",
			CheckpointClaim: "run-1-ckpts", APIKeySecret: "vllm-key",
			Args:      []string{"--max-model-len", "4096"},
			Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{"nvidia.com/gpu": resourceQuantity("1")}},
		},
	}
	r, c := checkpointReconciler(t, cs)
	reconcile(t, r, "team-a", "step-200")

	ctx := context.Background()
	dep := &appsv1.Deployment{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "evalsi-ckpt-step-200"}, dep); err != nil {
		t.Fatal(err)
	}
	pod := dep.Spec.Template.Spec
	cont := pod.Containers[0]
	if got := strings.Join(cont.Args, " "); got != "/checkpoints/run-1/step-200 --host 0.0.0.0 --port 8000 --served-model-name step-200 --max-model-len 4096" {
		t.Errorf("args = %s", got)
	}
	if dep.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType || *dep.Spec.Replicas != 1 {
		t.Errorf("strategy %v, replicas %v", dep.Spec.Strategy.Type, dep.Spec.Replicas)
	}
	mounts := map[string]corev1.VolumeMount{}
	for _, m := range cont.VolumeMounts {
		mounts[m.Name] = m
	}
	volumes := map[string]corev1.Volume{}
	for _, v := range pod.Volumes {
		volumes[v.Name] = v
	}
	if m := mounts["checkpoints"]; m.MountPath != "/checkpoints" || !m.ReadOnly || !volumes["checkpoints"].PersistentVolumeClaim.ReadOnly || volumes["checkpoints"].PersistentVolumeClaim.ClaimName != "run-1-ckpts" {
		t.Errorf("checkpoint volume: %+v %+v", mounts["checkpoints"], volumes["checkpoints"])
	}
	if volumes["shm"].EmptyDir == nil || volumes["shm"].EmptyDir.Medium != corev1.StorageMediumMemory || mounts["shm"].MountPath != "/dev/shm" {
		t.Errorf("no shared memory: %+v", volumes["shm"])
	}
	if cont.Resources.Limits["nvidia.com/gpu"] != resourceQuantity("1") {
		t.Errorf("resources %+v", cont.Resources)
	}
	// The key comes from the Secret, never the command line.
	var key *corev1.EnvVar
	for i := range cont.Env {
		if cont.Env[i].Name == "VLLM_API_KEY" {
			key = &cont.Env[i]
		}
	}
	if key == nil || key.ValueFrom.SecretKeyRef.Name != "vllm-key" || key.ValueFrom.SecretKeyRef.Key != "api-key" || strings.Contains(strings.Join(cont.Args, " "), "api-key") {
		t.Errorf("api key env = %+v", key)
	}
	if cont.StartupProbe == nil || cont.StartupProbe.HTTPGet.Path != "/health" || cont.ReadinessProbe == nil {
		t.Errorf("probes: %+v %+v", cont.StartupProbe, cont.ReadinessProbe)
	}
	if *cont.SecurityContext.AllowPrivilegeEscalation || cont.SecurityContext.Capabilities.Drop[0] != "ALL" {
		t.Errorf("security context %+v", cont.SecurityContext)
	}

	svc := &corev1.Service{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "evalsi-ckpt-step-200"}, svc); err != nil {
		t.Fatal(err)
	}
	if svc.Spec.Ports[0].Port != 8000 || svc.Spec.Selector["app.kubernetes.io/instance"] != "step-200" {
		t.Errorf("service %+v", svc.Spec)
	}
	if len(dep.OwnerReferences) != 1 || dep.OwnerReferences[0].Name != "step-200" || len(svc.OwnerReferences) != 1 {
		t.Errorf("not owned: %v %v", dep.OwnerReferences, svc.OwnerReferences)
	}

	// Loading: not ready, with the endpoint already known.
	got := &v1.CheckpointServer{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(cs), got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Endpoint != "http://evalsi-ckpt-step-200.team-a.svc:8000/v1" || got.Status.ServedModelName != "step-200" {
		t.Errorf("status = %+v", got.Status)
	}
	if !meta.IsStatusConditionFalse(got.Status.Conditions, "Ready") {
		t.Errorf("ready while loading: %+v", got.Status.Conditions)
	}

	// Once a replica passes the readiness probe it is Ready.
	dep.Status.ReadyReplicas, dep.Status.UpdatedReplicas = 1, 1
	if err := c.Status().Update(ctx, dep); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r, "team-a", "step-200")
	if err := c.Get(ctx, client.ObjectKeyFromObject(cs), got); err != nil {
		t.Fatal(err)
	}
	if !meta.IsStatusConditionTrue(got.Status.Conditions, "Ready") || got.Status.ReadyReplicas != 1 {
		t.Errorf("not ready: %+v", got.Status)
	}
}

// A LoRA server serves a base model that takes adapters loaded at runtime.
func TestCheckpointServerForLoRA(t *testing.T) {
	cs := &v1.CheckpointServer{
		ObjectMeta: metav1.ObjectMeta{Name: "base", Namespace: "team-a"},
		Spec: v1.CheckpointServerSpec{
			Image: "vllm/vllm-openai:v0.11.0", Model: "Qwen/Qwen3-8B", LoRA: true, ServedModelName: "qwen3", Port: 9000,
			Env: []corev1.EnvVar{{Name: "HF_TOKEN", Value: "x"}},
		},
	}
	r, c := checkpointReconciler(t, cs)
	reconcile(t, r, "team-a", "base")
	dep := &appsv1.Deployment{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: "evalsi-ckpt-base"}, dep); err != nil {
		t.Fatal(err)
	}
	cont := dep.Spec.Template.Spec.Containers[0]
	if got := strings.Join(cont.Args, " "); got != "Qwen/Qwen3-8B --host 0.0.0.0 --port 9000 --served-model-name qwen3 --enable-lora" {
		t.Errorf("args = %s", got)
	}
	env := map[string]string{}
	for _, e := range cont.Env {
		env[e.Name] = e.Value
	}
	if env["VLLM_ALLOW_RUNTIME_LORA_UPDATING"] != "True" || env["HF_TOKEN"] != "x" {
		t.Errorf("env = %v", env)
	}
	got := &v1.CheckpointServer{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(cs), got); err != nil || got.Status.Endpoint != "http://evalsi-ckpt-base.team-a.svc:9000/v1" || got.Status.ServedModelName != "qwen3" {
		t.Errorf("status = %+v, %v", got.Status, err)
	}
	// No claim, no checkpoint volume.
	for _, v := range dep.Spec.Template.Spec.Volumes {
		if v.Name == "checkpoints" {
			t.Error("a checkpoint volume without a claim")
		}
	}
}

// Changing the spec changes the Deployment; a missing resource is not an error.
func TestCheckpointServerFollowsItsSpec(t *testing.T) {
	cs := &v1.CheckpointServer{
		ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "ns"},
		Spec:       v1.CheckpointServerSpec{Image: "vllm:1", Model: "a"},
	}
	r, c := checkpointReconciler(t, cs)
	reconcile(t, r, "ns", "m")
	reconcile(t, r, "ns", "gone") // deleted since it was queued
	ctx := context.Background()
	if err := c.Get(ctx, client.ObjectKeyFromObject(cs), cs); err != nil {
		t.Fatal(err)
	}
	cs.Spec.Model, cs.Spec.Replicas = "b", func() *int32 { n := int32(2); return &n }()
	if err := c.Update(ctx, cs); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r, "ns", "m")
	dep := &appsv1.Deployment{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "evalsi-ckpt-m"}, dep); err != nil {
		t.Fatal(err)
	}
	if dep.Spec.Template.Spec.Containers[0].Args[0] != "b" || *dep.Spec.Replicas != 2 {
		t.Errorf("deployment did not follow the spec: %v %v", dep.Spec.Template.Spec.Containers[0].Args, *dep.Spec.Replicas)
	}
}

func resourceQuantity(s string) resource.Quantity { return resource.MustParse(s) }

// After a spec change the Deployment's status still describes the old
// revision: replicas of the old model are ready, but the new one is not.
func TestCheckpointServerIsNotReadyUntilTheNewRevisionRolledOut(t *testing.T) {
	cs := &v1.CheckpointServer{
		ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "ns"},
		Spec:       v1.CheckpointServerSpec{Image: "vllm:1", Model: "a"},
	}
	r, c := checkpointReconciler(t, cs)
	reconcile(t, r, "ns", "m")
	ctx := context.Background()
	key := client.ObjectKey{Namespace: "ns", Name: "evalsi-ckpt-m"}
	dep := &appsv1.Deployment{}
	if err := c.Get(ctx, key, dep); err != nil {
		t.Fatal(err)
	}
	// Generation 2 is the new spec; the controller of Deployments has seen
	// generation 1 only, whose replica is ready.
	dep.Generation = 2
	if err := c.Update(ctx, dep); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, key, dep); err != nil {
		t.Fatal(err)
	}
	dep.Status = appsv1.DeploymentStatus{ObservedGeneration: 1, ReadyReplicas: 1, UpdatedReplicas: 1}
	if err := c.Status().Update(ctx, dep); err != nil {
		t.Fatal(err)
	}
	ready := func() bool {
		reconcile(t, r, "ns", "m")
		got := &v1.CheckpointServer{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(cs), got); err != nil {
			t.Fatal(err)
		}
		return meta.IsStatusConditionTrue(got.Status.Conditions, "Ready")
	}
	if ready() {
		t.Error("Ready on the previous revision's replica")
	}
	// The new revision is observed but its replica has not started.
	dep.Status = appsv1.DeploymentStatus{ObservedGeneration: 2, UpdatedReplicas: 1, ReadyReplicas: 0}
	if err := c.Status().Update(ctx, dep); err != nil {
		t.Fatal(err)
	}
	if ready() {
		t.Error("Ready before any replica of the new revision is")
	}
	dep.Status = appsv1.DeploymentStatus{ObservedGeneration: 2, UpdatedReplicas: 1, ReadyReplicas: 1}
	if err := c.Status().Update(ctx, dep); err != nil {
		t.Fatal(err)
	}
	if !ready() {
		t.Error("not Ready once the new revision's replica is")
	}
}
