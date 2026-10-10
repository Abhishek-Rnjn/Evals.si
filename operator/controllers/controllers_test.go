package controllers

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/abhishek-rnjn/evals.si/internal/config"
	v1 "github.com/abhishek-rnjn/evals.si/operator/api/v1alpha1"
)

// example reads one of the repository's example files as a resource.
func example(t *testing.T, path, namespace string) *unstructured.Unstructured {
	t.Helper()
	data, err := os.ReadFile("../../examples/" + path)
	if err != nil {
		t.Fatal(err)
	}
	obj := &unstructured.Unstructured{}
	if err := yaml.Unmarshal(data, &obj.Object); err != nil {
		t.Fatal(err)
	}
	obj.SetNamespace(namespace)
	return obj
}

func namespace(t *testing.T, c client.Client, name string) {
	t.Helper()
	if err := c.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
		t.Fatal(err)
	}
}

func TestOperator(t *testing.T) {
	e := start(t)
	ctx := context.Background()
	namespace(t, e.admin, "team-a")
	namespace(t, e.admin, "evalsi")
	alice := e.as("alice")

	t.Run("EvalRun", func(t *testing.T) {
		// The CLI's run file applies unchanged; the creator is recorded
		// whatever the file claims.
		obj := example(t, "runs/capitals.yaml", "team-a")
		obj.SetAnnotations(map[string]string{v1.CreatedByAnnotation: "mallory"})
		if err := alice.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
		run := &v1.EvalRun{}
		key := client.ObjectKey{Namespace: "team-a", Name: "capitals"}
		eventually(t, "the run to succeed", func() bool {
			return e.admin.Get(ctx, key, run) == nil && run.Status.Phase == PhaseSucceeded
		})
		if got := run.Annotations[v1.CreatedByAnnotation]; got != "alice" {
			t.Errorf("created-by %q", got)
		}
		e.api.mu.Lock()
		req := e.api.creates[0]
		token := e.api.tokens[0]
		e.api.mu.Unlock()
		if req.GetName() != "capitals" || req.GetProject() != "quickstart" || req.GetSpec().GetTrials() != 3 || len(req.GetLabels()) != 0 {
			t.Errorf("CreateRun %v", req)
		}
		if token != "Bearer sa-token" {
			t.Errorf("token %q", token)
		}
		s := run.Status
		if s.RunID != "run-a" || s.Progress.Done != 15 || len(s.Summaries) != 1 || s.Summaries[0].Mean != "0.933333" || s.Summaries[0].CILow != "0.7" || !s.Gates[0].Passed || s.FinishedAt == nil {
			t.Errorf("status %+v", s)
		}
		if !meta.IsStatusConditionTrue(s.Conditions, "Succeeded") || !meta.IsStatusConditionTrue(s.Conditions, "Accepted") {
			t.Errorf("conditions %+v", s.Conditions)
		}

		// The spec cannot change, nor the creator.
		patch := client.MergeFrom(run.DeepCopy())
		run.Spec = runtime.RawExtension{Raw: []byte(`{"evaluators":[{"ref":"x"}],"dataset":{"path":"y"}}`)}
		if err := alice.Patch(ctx, run, patch); err == nil || !strings.Contains(err.Error(), "cannot change") {
			t.Errorf("spec update: %v", err)
		}
		_ = e.admin.Get(ctx, key, run)
		patch = client.MergeFrom(run.DeepCopy())
		run.Annotations[v1.CreatedByAnnotation] = "mallory"
		run.Labels["team"] = "a"
		if err := alice.Patch(ctx, run, patch); err != nil {
			t.Fatal(err)
		}
		_ = e.admin.Get(ctx, key, run)
		if run.Annotations[v1.CreatedByAnnotation] != "alice" || run.Labels["team"] != "a" {
			t.Errorf("after update: %v %v", run.Annotations, run.Labels)
		}
	})

	t.Run("InvalidSpecs", func(t *testing.T) {
		bad := &v1.EvalRun{ObjectMeta: metav1.ObjectMeta{Name: "bad", Namespace: "team-a"}, Spec: runtime.RawExtension{Raw: []byte(`{"evaluators":[{"ref":"x"}],"dataset":{"path":"a"},"gates":[{"metric":"x"}]}`)}}
		if err := alice.Create(ctx, bad); err == nil || !strings.Contains(err.Error(), "needs min or max") {
			t.Errorf("invalid run: %v", err)
		}
		bad.Spec.Raw = []byte(`{"evaluators":[{"ref":"x"}],"dataset":{"path":"a"},"trails":3}`)
		if err := alice.Create(ctx, bad); err == nil || !strings.Contains(err.Error(), "unknown field") {
			t.Errorf("misspelled field: %v", err)
		}
		// The CRD schema, typed from the API's messages, rejects wrong types itself.
		bad.Spec.Raw = []byte(`{"evaluators":[{"ref":"x"}],"dataset":{"path":"a"},"trials":"three"}`)
		if err := alice.Create(ctx, bad); err == nil || !strings.Contains(err.Error(), "spec.trials") {
			t.Errorf("wrong type: %v", err)
		}
		// What evalsid would refuse (a variable the project has no grant
		// for) is refused at admission, not later in the run's status.
		secret := &v1.EvalRun{
			ObjectMeta: metav1.ObjectMeta{Name: "exfil", Namespace: "team-a", Labels: map[string]string{v1.ProjectLabel: "quickstart"}},
			Spec:       runtime.RawExtension{Raw: []byte(`{"target":{"connector":"openai-compatible","model":"m","base_url":"https://x.example/v1","api_key_env":"DATABASE_URL"},"evaluators":[{"ref":"exact-match"}],"dataset":{"path":"a.jsonl"}}`)},
		}
		if err := alice.Create(ctx, secret); err == nil || !strings.Contains(err.Error(), "may not use") {
			t.Errorf("ungranted variable: %v", err)
		}
		e.api.mu.Lock()
		validated := slices.Clone(e.api.validated)
		e.api.mu.Unlock()
		if !slices.Contains(validated, "run/exfil") {
			t.Errorf("the webhook did not ask the API: %v", validated)
		}
		pol := &v1.OnlineEvalPolicy{ObjectMeta: metav1.ObjectMeta{Name: "bad", Namespace: "team-a"}, Spec: runtime.RawExtension{Raw: []byte(`{"stages":[]}`)}}
		if err := alice.Create(ctx, pol); err == nil || !strings.Contains(err.Error(), "at least one stage") {
			t.Errorf("invalid policy: %v", err)
		}
	})

	t.Run("DeletingCancels", func(t *testing.T) {
		run := &v1.EvalRun{ObjectMeta: metav1.ObjectMeta{Name: "long", Namespace: "team-a"}, Spec: runtime.RawExtension{Raw: []byte(`{"evaluators":[{"ref":"exact-match"}],"dataset":{"path":"a.jsonl"},"judge":"hold"}`)}}
		if err := alice.Create(ctx, run); err != nil {
			t.Fatal(err)
		}
		key := client.ObjectKeyFromObject(run)
		eventually(t, "the run to start", func() bool {
			return e.admin.Get(ctx, key, run) == nil && run.Status.Phase == PhaseRunning
		})
		if run.Status.Project != "team-a" {
			t.Errorf("project %q: the namespace is the default", run.Status.Project)
		}
		e.api.mu.Lock()
		created := e.api.projects["team-a"]
		e.api.mu.Unlock()
		if !created {
			t.Error("the namespace's project was not created")
		}
		if err := alice.Delete(ctx, run); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the resource to go", func() bool { return e.admin.Get(ctx, key, run) != nil })
		e.api.mu.Lock()
		defer e.api.mu.Unlock()
		if !slices.Equal(e.api.cancelled, []string{run.Status.RunID}) {
			t.Errorf("cancelled %v", e.api.cancelled)
		}
	})

	t.Run("OnlineEvalPolicy", func(t *testing.T) {
		obj := example(t, "watch/support-policy.yaml", "team-a")
		if err := alice.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
		p := &v1.OnlineEvalPolicy{}
		key := client.ObjectKey{Namespace: "team-a", Name: "support-agent"}
		eventually(t, "the policy's counters", func() bool {
			return e.admin.Get(ctx, key, p) == nil && p.Status.TracesSeen == 40
		})
		e.api.mu.Lock()
		got := e.api.policies["support-agent"]
		e.api.mu.Unlock()
		if got.GetProject() != "support" || got.GetWindow().AsDuration().Seconds() != 300 || got.GetSampling().GetRate() != 0.1 || len(got.GetStages()) != 2 {
			t.Errorf("applied %v", got)
		}
		if !meta.IsStatusConditionTrue(p.Status.Conditions, "Synced") || p.Status.TracesEvaluated != 4 || p.Annotations[v1.CreatedByAnnotation] != "alice" {
			t.Errorf("status %+v", p.Status)
		}

		// A change is applied again.
		patch := client.MergeFrom(p.DeepCopy())
		p.Spec.Raw = []byte(strings.Replace(string(p.Spec.Raw), `"rate":0.1`, `"rate":0.5`, 1))
		if err := alice.Patch(ctx, p, patch); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the change to apply", func() bool {
			e.api.mu.Lock()
			defer e.api.mu.Unlock()
			return e.api.policies["support-agent"].GetSampling().GetRate() == 0.5
		})

		// The same name in another project conflicts rather than taking over.
		other := example(t, "watch/support-policy.yaml", "evalsi")
		other.SetLabels(nil)
		if err := alice.Create(ctx, other); err != nil {
			t.Fatal(err)
		}
		p2 := &v1.OnlineEvalPolicy{}
		eventually(t, "the conflict", func() bool {
			_ = e.admin.Get(ctx, client.ObjectKeyFromObject(other), p2)
			c := meta.FindStatusCondition(p2.Status.Conditions, "Synced")
			return c != nil && c.Reason == "Conflict"
		})
		_ = alice.Delete(ctx, other)

		// Deleting the resource deletes the policy.
		if err := alice.Delete(ctx, p); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the policy to go", func() bool {
			e.api.mu.Lock()
			defer e.api.mu.Unlock()
			return e.api.policies["support-agent"] == nil && e.admin.Get(ctx, key, p) != nil
		})
	})

	t.Run("TraceSource", func(t *testing.T) {
		obj := example(t, "sources/studio-mlflow.yaml", "team-a")
		if err := alice.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
		s := &v1.TraceSource{}
		key := client.ObjectKey{Namespace: "team-a", Name: "studio-mlflow"}
		eventually(t, "the source's status", func() bool {
			return e.admin.Get(ctx, key, s) == nil && s.Status.Pulled == 7
		})
		e.api.mu.Lock()
		got := e.api.sources["support/studio-mlflow"]
		validated := slices.Contains(e.api.validated, "source/studio-mlflow")
		e.api.mu.Unlock()
		if got.GetConnector() != "mlflow" || got.GetBackfill().GetSince().AsDuration().Hours() != 720 || got.GetCredentials().GetFile() != "mlflow-token/token" || !got.GetWriteBack().GetEnabled() {
			t.Errorf("applied %v", got)
		}
		if !validated {
			t.Error("admission did not ask the API (validate_only)")
		}
		if !meta.IsStatusConditionTrue(s.Status.Conditions, "Synced") || s.Status.Phase != "Tailing" || s.Status.Scored != 5 || s.Status.LagSeconds != "12" || s.Status.Watermark == nil {
			t.Errorf("status %+v", s.Status)
		}

		// What the API would refuse is refused at kubectl apply.
		bad := example(t, "sources/studio-mlflow.yaml", "team-a")
		bad.SetName("metadata")
		spec := bad.Object["spec"].(map[string]any)
		spec["endpoint"] = "http://169.254.169.254"
		if err := alice.Create(ctx, bad); err == nil || !strings.Contains(err.Error(), "private, loopback") {
			t.Fatalf("a metadata endpoint was admitted: %v", err)
		}
		// So is a spec that sets what metadata owns.
		bad2 := example(t, "sources/studio-mlflow.yaml", "team-a")
		bad2.SetName("named")
		bad2.Object["spec"].(map[string]any)["name"] = "other"
		if err := alice.Create(ctx, bad2); err == nil || !strings.Contains(err.Error(), "metadata") {
			t.Fatalf("a spec naming itself was admitted: %v", err)
		}

		// Deleting the resource deletes the source.
		if err := alice.Delete(ctx, s); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the source to go", func() bool {
			e.api.mu.Lock()
			defer e.api.mu.Unlock()
			return e.api.sources["support/studio-mlflow"] == nil && e.admin.Get(ctx, key, s) != nil
		})
	})

	t.Run("Evaluator", func(t *testing.T) {
		ev := &v1.Evaluator{
			ObjectMeta: metav1.ObjectMeta{Name: "judges", Namespace: "team-a"},
			Spec: v1.EvaluatorSpec{
				Image: "ghcr.io/example/judges:1", Pools: []string{"judge", "cpu"}, Concurrency: 8,
				Autoscaling: &v1.Autoscaling{MinReplicas: 1, MaxReplicas: 20, LagThreshold: 5},
			},
		}
		if err := alice.Create(ctx, ev); err != nil {
			t.Fatal(err)
		}
		dep := &appsv1.Deployment{}
		key := client.ObjectKey{Namespace: "team-a", Name: "evalsi-evaluator-judges"}
		eventually(t, "the worker Deployment", func() bool { return e.admin.Get(ctx, key, dep) == nil })
		c := dep.Spec.Template.Spec.Containers[0]
		if c.Image != "ghcr.io/example/judges:1" || strings.Join(c.Args, " ") != "worker --pools judge,cpu --config /etc/evalsi/evalsi.yaml --concurrency 8" {
			t.Errorf("container %s %v", c.Image, c.Args)
		}
		volumes := map[string]corev1.Volume{}
		for _, v := range dep.Spec.Template.Spec.Volumes {
			volumes[v.Name] = v
		}
		mounts := map[string]string{}
		for _, m := range c.VolumeMounts {
			mounts[m.Name] = m.MountPath
		}
		if volumes["config"].ConfigMap.Name != "evalsi-worker" || *c.SecurityContext.ReadOnlyRootFilesystem != true {
			t.Errorf("pod %+v", dep.Spec.Template.Spec)
		}
		// A writable home (the judge cache) under the read-only root, and
		// what the release's worker config refers to.
		if volumes["home"].EmptyDir == nil || mounts["home"] != "/var/lib/evalsi" {
			t.Errorf("no writable home: %+v %v", volumes, mounts)
		}
		if volumes["worker-tls"].Secret.SecretName != "evalsi-worker-tls" || mounts["worker-tls"] != "/etc/evalsi/worker-tls" {
			t.Errorf("no sandbox client certificate: %+v %v", volumes, mounts)
		}
		if len(c.Env) < 2 || c.Env[0].ValueFrom.SecretKeyRef.Name != "evalsi-s3" || c.Env[1].Name != "EVALSI_S3_SECRET_KEY" {
			t.Errorf("no S3 credentials: %+v", c.Env)
		}
		so := &unstructured.Unstructured{}
		so.SetGroupVersionKind(scaledObjectGVK)
		eventually(t, "the ScaledObject", func() bool { return e.admin.Get(ctx, key, so) == nil })
		triggers, _, _ := unstructured.NestedSlice(so.Object, "spec", "triggers")
		md := triggers[0].(map[string]any)["metadata"].(map[string]any)
		if len(triggers) != 2 || md["consumer"] != "pool-judge" || md["stream"] != "EVALSI_WORK" || md["lagThreshold"] != "5" || md["natsServerMonitoringEndpoint"] != "evalsi-nats.evalsi.svc:8222" {
			t.Errorf("triggers %v", triggers)
		}
		if max, _, _ := unstructured.NestedInt64(so.Object, "spec", "maxReplicaCount"); max != 20 {
			t.Errorf("max %d", max)
		}
		// KEDA owns the replica count from here.
		dep.Spec.Replicas = ptr.To(int32(7))
		if err := e.admin.Update(ctx, dep); err != nil {
			t.Fatal(err)
		}
		_ = e.admin.Get(ctx, client.ObjectKeyFromObject(ev), ev)
		ev.Spec.Concurrency = 2
		if err := e.admin.Update(ctx, ev); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the change", func() bool {
			_ = e.admin.Get(ctx, key, dep)
			return slices.Contains(dep.Spec.Template.Spec.Containers[0].Args, "2")
		})
		if *dep.Spec.Replicas != 7 {
			t.Errorf("replicas reset to %d", *dep.Spec.Replicas)
		}
		eventually(t, "status", func() bool {
			_ = e.admin.Get(ctx, client.ObjectKeyFromObject(ev), ev)
			return meta.IsStatusConditionTrue(ev.Status.Conditions, "Autoscaling")
		})
	})

	t.Run("SandboxClass", func(t *testing.T) {
		for _, sc := range []*v1.SandboxClass{
			{ObjectMeta: metav1.ObjectMeta{Name: "bwrap"}, Spec: v1.SandboxClassSpec{MinIsolation: "namespaced", MaxSandboxes: 16, Replicas: ptr.To(int32(2))}},
			{ObjectMeta: metav1.ObjectMeta{Name: "gvisor"}, Spec: v1.SandboxClassSpec{Ladder: []string{"pod"}, MinIsolation: "kernel", Pod: &v1.PodRung{RuntimeClassName: "gvisor", Level: "kernel", DefaultImage: "python:3.13-slim", NetworkPolicyEnforced: true, RunAsUser: ptr.To(int64(1000)), Capabilities: &[]string{}}}},
			{ObjectMeta: metav1.ObjectMeta{Name: "landlock"}, Spec: v1.SandboxClassSpec{Ladder: []string{"landlock"}}},
		} {
			if err := alice.Create(ctx, sc); err != nil {
				t.Fatal(err)
			}
		}
		get := func(name string) (*v1.SandboxClass, *appsv1.Deployment, config.Config) {
			sc, dep, cm := &v1.SandboxClass{}, &appsv1.Deployment{}, &corev1.ConfigMap{}
			key := client.ObjectKey{Namespace: "evalsi", Name: "evalsi-sandbox-" + name}
			eventually(t, "the pool "+name, func() bool {
				return e.admin.Get(ctx, client.ObjectKey{Name: name}, sc) == nil && sc.Status.Address != "" &&
					e.admin.Get(ctx, key, dep) == nil && e.admin.Get(ctx, key, cm) == nil &&
					e.admin.Get(ctx, key, &corev1.Service{}) == nil
			})
			// The rendered file is a valid evalsi.yaml.
			f := t.TempDir() + "/evalsi.yaml"
			_ = os.WriteFile(f, []byte(cm.Data["evalsi.yaml"]), 0o600)
			cfg, err := config.Load(f)
			if err != nil {
				t.Fatalf("%s: %v\n%s", name, err, cm.Data["evalsi.yaml"])
			}
			return sc, dep, cfg
		}

		sc, dep, cfg := get("bwrap")
		if sc.Status.Address != "tls://evalsi-sandbox-bwrap.evalsi.svc:7443" || sc.Annotations[v1.CreatedByAnnotation] != "alice" {
			t.Errorf("status %+v %v", sc.Status, sc.Annotations)
		}
		if !slices.Equal(cfg.Sandbox.Ladder, []string{"bwrap", "landlock"}) || cfg.Sandbox.MinIsolation != "namespaced" || cfg.Sandbox.MaxSandboxes != 16 {
			t.Errorf("config %+v", cfg.Sandbox)
		}
		pod := dep.Spec.Template.Spec
		c := pod.Containers[0]
		if *dep.Spec.Replicas != 2 || !slices.Contains(c.Args, "spiffe://evals.si/ns/evalsi/sa/evalsi-worker") || c.Image != "ghcr.io/abhishek-rnjn/evalsi:test" {
			t.Errorf("deployment %v %v", c.Args, c.Image)
		}
		if pod.HostUsers == nil || *pod.HostUsers || *c.SecurityContext.ProcMount != corev1.UnmaskedProcMount || *pod.AutomountServiceAccountToken {
			t.Errorf("bwrap pool security %+v %+v", pod, c.SecurityContext)
		}

		_, dep, cfg = get("gvisor")
		p := cfg.Sandbox.Pod
		if p == nil || p.RuntimeClassName != "gvisor" || p.GuestImage != "ghcr.io/abhishek-rnjn/evalsi:test" || p.Namespace != "evalsi" || p.Labels["evals.si/sandbox-class"] != "gvisor" {
			t.Errorf("pod rung %+v", p)
		}
		pod = dep.Spec.Template.Spec
		c = pod.Containers[0]
		if pod.HostUsers != nil || !*pod.SecurityContext.RunAsNonRoot || *c.SecurityContext.AllowPrivilegeEscalation || !*pod.AutomountServiceAccountToken || pod.ServiceAccountName != "evalsi-sandboxd" {
			t.Errorf("pod-rung pool security %+v", pod)
		}
		if *p.RunAsUser != 1000 || p.Capabilities == nil || len(*p.Capabilities) != 0 {
			t.Errorf("pod rung user %v, capabilities %v", p.RunAsUser, p.Capabilities)
		}

		// Landlock alone needs no privilege: the pool runs restricted.
		_, dep, _ = get("landlock")
		pod = dep.Spec.Template.Spec
		c = pod.Containers[0]
		if pod.HostUsers != nil || !*pod.SecurityContext.RunAsNonRoot || c.SecurityContext.Privileged != nil || *pod.AutomountServiceAccountToken {
			t.Errorf("landlock pool security %+v %+v", pod, c.SecurityContext)
		}
	})
}
