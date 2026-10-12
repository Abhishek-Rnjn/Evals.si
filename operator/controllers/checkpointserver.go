package controllers

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	v1 "github.com/abhishek-rnjn/evals.si/operator/api/v1alpha1"
)

const (
	defaultServerPort = 8000
	// Where CheckpointServer.spec.checkpointClaim is mounted.
	checkpointMount = "/checkpoints"
	// APIKeyKey is the key of the Secret named by spec.apiKeySecret.
	APIKeyKey = "api-key"
	// vLLM reads the key from this variable; the operator never puts it on a command line.
	vllmAPIKeyEnv = "VLLM_API_KEY"
	// vLLM loads a model for minutes: the startup probe allows 30.
	startupFailures = 180
	startupPeriod   = 10
)

// CheckpointServerReconciler runs each CheckpointServer as a vLLM Deployment
// and Service, and reports the endpoint and readiness in its status.
type CheckpointServerReconciler struct {
	client.Client
	// Names of what it makes start with this (default "evalsi").
	NamePrefix string
}

// +kubebuilder:rbac:groups=evals.si,resources=checkpointservers,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=evals.si,resources=checkpointservers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete

func (r *CheckpointServerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	cs := &v1.CheckpointServer{}
	if err := r.Get(ctx, req.NamespacedName, cs); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	name := childName(r.NamePrefix, "ckpt", cs.Name)
	labels := map[string]string{"app.kubernetes.io/name": "evalsi-checkpoint-server", "app.kubernetes.io/instance": cs.Name, "app.kubernetes.io/managed-by": "evalsi-operator"}
	port := cs.Spec.Port
	if port == 0 {
		port = defaultServerPort
	}
	served := cs.Spec.ServedModelName
	if served == "" {
		served = cs.Name
	}

	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cs.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		dep.Labels = labels
		dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
		dep.Spec.Replicas = ptr.To(ptr.Deref(cs.Spec.Replicas, 1))
		// A model loads onto the GPUs it holds: replacing a pod means freeing them first.
		dep.Spec.Strategy = appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
		dep.Spec.Template = serverPod(cs, labels, served, port)
		return controllerutil.SetControllerReference(cs, dep, r.Scheme())
	}); err != nil {
		return ctrl.Result{}, err
	}

	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cs.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = labels
		svc.Spec.Selector = labels
		svc.Spec.Ports = []corev1.ServicePort{{Name: "http", Port: port, TargetPort: intstr.FromString("http")}}
		return controllerutil.SetControllerReference(cs, svc, r.Scheme())
	}); err != nil {
		return ctrl.Result{}, err
	}

	cs.Status.Endpoint = fmt.Sprintf("http://%s.%s.svc:%d/v1", name, cs.Namespace, port)
	cs.Status.ServedModelName = served
	cs.Status.ReadyReplicas = dep.Status.ReadyReplicas
	cs.Status.ObservedGeneration = cs.Generation
	ready := metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, Reason: "Loading", Message: "vLLM is loading the model", ObservedGeneration: cs.Generation}
	if deploymentCurrentAndReady(dep) {
		ready.Status, ready.Reason, ready.Message = metav1.ConditionTrue, "Serving", "vLLM answers /health"
	}
	meta.SetStatusCondition(&cs.Status.Conditions, ready)
	return ctrl.Result{}, r.Status().Update(ctx, cs)
}

// deploymentCurrentAndReady reports whether the Deployment has rolled out its
// current spec and a replica of it passes the readiness probe. Right after a
// spec change the status still describes the previous revision, whose
// replicas are ready: that must not read as the new model being served.
func deploymentCurrentAndReady(dep *appsv1.Deployment) bool {
	want := ptr.Deref(dep.Spec.Replicas, 1)
	return dep.Status.ObservedGeneration >= dep.Generation && dep.Status.UpdatedReplicas >= want && dep.Status.ReadyReplicas > 0
}

// serverPod is the pod template of a CheckpointServer.
func serverPod(cs *v1.CheckpointServer, labels map[string]string, served string, port int32) corev1.PodTemplateSpec {
	args := []string{cs.Spec.Model, "--host", "0.0.0.0", "--port", fmt.Sprint(port), "--served-model-name", served}
	env := append([]corev1.EnvVar{}, cs.Spec.Env...)
	if cs.Spec.LoRA {
		args = append(args, "--enable-lora")
		env = append(env, corev1.EnvVar{Name: "VLLM_ALLOW_RUNTIME_LORA_UPDATING", Value: "True"})
	}
	if cs.Spec.APIKeySecret != "" {
		env = append(env, corev1.EnvVar{Name: vllmAPIKeyEnv, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: cs.Spec.APIKeySecret}, Key: APIKeyKey,
		}}})
	}
	args = append(args, cs.Spec.Args...)

	// vLLM shares memory between its processes through /dev/shm, which a
	// container's default is far too small for.
	volumes := []corev1.Volume{
		{Name: "shm", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}}},
		{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}
	mounts := []corev1.VolumeMount{{Name: "shm", MountPath: "/dev/shm"}, {Name: "tmp", MountPath: "/tmp"}}
	if cs.Spec.CheckpointClaim != "" {
		volumes = append(volumes, corev1.Volume{Name: "checkpoints", VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: cs.Spec.CheckpointClaim, ReadOnly: true},
		}})
		mounts = append(mounts, corev1.VolumeMount{Name: "checkpoints", MountPath: checkpointMount, ReadOnly: true})
	}
	health := corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/health", Port: intstr.FromString("http")}}
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: labels},
		Spec: corev1.PodSpec{
			ServiceAccountName:           cs.Spec.ServiceAccountName,
			AutomountServiceAccountToken: ptr.To(false),
			NodeSelector:                 cs.Spec.NodeSelector,
			Tolerations:                  cs.Spec.Tolerations,
			Volumes:                      volumes,
			Containers: []corev1.Container{{
				Name: "vllm", Image: cs.Spec.Image, Command: cs.Spec.Command, Args: args, Env: env,
				Resources:    cs.Spec.Resources,
				Ports:        []corev1.ContainerPort{{Name: "http", ContainerPort: port}},
				VolumeMounts: mounts,
				// vLLM images run as root and write caches, so the root stays
				// writable; it needs nothing else.
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: ptr.To(false),
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
					SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
				},
				StartupProbe:   &corev1.Probe{ProbeHandler: health, PeriodSeconds: startupPeriod, FailureThreshold: startupFailures},
				ReadinessProbe: &corev1.Probe{ProbeHandler: health, PeriodSeconds: 10},
			}},
		},
	}
}

func (r *CheckpointServerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&v1.CheckpointServer{}).
		Owns(&appsv1.Deployment{}).Owns(&corev1.Service{}).Named("checkpointserver").Complete(r)
}
