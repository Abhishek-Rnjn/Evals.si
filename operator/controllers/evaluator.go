package controllers

import (
	"context"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	v1 "github.com/abhishek-rnjn/evals.si/operator/api/v1alpha1"
)

// The work stream and its consumers (internal/cluster).
const (
	workStream     = "EVALSI_WORK"
	consumerPrefix = "pool-"
	configMount    = "/etc/evalsi"
	workerHome     = "/var/lib/evalsi"
)

var scaledObjectGVK = schema.GroupVersionKind{Group: "keda.sh", Version: "v1alpha1", Kind: "ScaledObject"}

// EvaluatorReconciler runs each Evaluator as a worker Deployment, scaled by
// KEDA on its pools' backlog when asked.
type EvaluatorReconciler struct {
	client.Client
	// The ConfigMap with the workers' evalsi.yaml (cluster section, judges,
	// sandbox service) when an Evaluator names none; it must be in the
	// Evaluator's namespace.
	WorkerConfigMap string
	// Secrets workers using WorkerConfigMap need, as the chart's workers
	// get them: the client certificate for remote sandbox pools (mounted at
	// /etc/evalsi/worker-tls) and S3 credentials (access_key, secret_key).
	// An Evaluator with its own ConfigMap brings its own, through env.
	WorkerTLSSecret string
	WorkerS3Secret  string
	// Names of the Deployments it makes start with this (default "evalsi").
	NamePrefix string
	// NATS's monitoring endpoint (host:8222), which KEDA's JetStream scaler reads.
	NATSMonitoringEndpoint string
}

// +kubebuilder:rbac:groups=evals.si,resources=evaluators,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=evals.si,resources=evaluators/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=keda.sh,resources=scaledobjects,verbs=get;list;watch;create;update;patch;delete

func (r *EvaluatorReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ev := &v1.Evaluator{}
	if err := r.Get(ctx, req.NamespacedName, ev); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	name := childName(r.NamePrefix, "evaluator", ev.Name)
	labels := map[string]string{"app.kubernetes.io/name": "evalsi-worker", "app.kubernetes.io/instance": ev.Name, "app.kubernetes.io/managed-by": "evalsi-operator"}
	pools := ev.Spec.Pools
	if len(pools) == 0 {
		pools = []string{"cpu"}
	}
	configMap := ev.Spec.ConfigMap
	if configMap == "" {
		configMap = r.WorkerConfigMap
	}

	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ev.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		dep.Labels = labels
		dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
		// Under KEDA the ScaledObject owns the replica count.
		if ev.Spec.Autoscaling == nil {
			dep.Spec.Replicas = ptr.To(ptr.Deref(ev.Spec.Replicas, 1))
		} else if dep.Spec.Replicas == nil {
			dep.Spec.Replicas = ptr.To(ev.Spec.Autoscaling.MinReplicas)
		}
		args := []string{"worker", "--pools", strings.Join(pools, ",")}
		if configMap != "" {
			args = append(args, "--config", configMount+"/evalsi.yaml")
		}
		if ev.Spec.Concurrency > 0 {
			args = append(args, "--concurrency", fmt.Sprint(ev.Spec.Concurrency))
		}
		pod := &dep.Spec.Template
		pod.Labels = labels
		pod.Spec.ServiceAccountName = ev.Spec.ServiceAccountName
		pod.Spec.NodeSelector = ev.Spec.NodeSelector
		pod.Spec.Tolerations = ev.Spec.Tolerations
		pod.Spec.SecurityContext = &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}}
		c := corev1.Container{
			Name: "worker", Image: ev.Spec.Image, Command: []string{"evalsid"}, Args: args,
			Env: ev.Spec.Env, Resources: ev.Spec.Resources,
			SecurityContext: &corev1.SecurityContext{
				AllowPrivilegeEscalation: ptr.To(false), ReadOnlyRootFilesystem: ptr.To(true),
				Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			},
			// The root stays read-only; the worker's home (its judge cache)
			// and /tmp are writable, as in the chart's workers.
			VolumeMounts: []corev1.VolumeMount{{Name: "home", MountPath: workerHome}, {Name: "tmp", MountPath: "/tmp"}},
		}
		emptyDir := corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}
		pod.Spec.Volumes = []corev1.Volume{{Name: "home", VolumeSource: emptyDir}, {Name: "tmp", VolumeSource: emptyDir}}
		if configMap != "" {
			c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "config", MountPath: configMount, ReadOnly: true})
			pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: "config", VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: configMap}},
			}})
		}
		if ev.Spec.ConfigMap == "" {
			r.addReleaseSecrets(pod, &c)
		}
		pod.Spec.Containers = []corev1.Container{c}
		return controllerutil.SetControllerReference(ev, dep, r.Scheme())
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	scaling := metav1.Condition{Type: "Autoscaling", Status: metav1.ConditionFalse, Reason: "NotRequested"}
	if ev.Spec.Autoscaling != nil {
		scaling = r.scaledObject(ctx, ev, name, pools)
	}
	ev.Status.ReadyReplicas = dep.Status.ReadyReplicas
	ev.Status.ObservedGeneration = ev.Generation
	ready := metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, Reason: "Starting", ObservedGeneration: ev.Generation}
	if dep.Status.ReadyReplicas > 0 {
		ready.Status, ready.Reason = metav1.ConditionTrue, "Running"
	}
	meta.SetStatusCondition(&ev.Status.Conditions, ready)
	scaling.ObservedGeneration = ev.Generation
	meta.SetStatusCondition(&ev.Status.Conditions, scaling)
	return ctrl.Result{}, r.Status().Update(ctx, ev)
}

// addReleaseSecrets gives a worker on the release's default ConfigMap what
// that config refers to: the sandbox client certificate and S3 credentials.
func (r *EvaluatorReconciler) addReleaseSecrets(pod *corev1.PodTemplateSpec, c *corev1.Container) {
	if r.WorkerTLSSecret != "" {
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "worker-tls", MountPath: configMount + "/worker-tls", ReadOnly: true})
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: "worker-tls", VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: r.WorkerTLSSecret},
		}})
	}
	if r.WorkerS3Secret != "" {
		secret := func(key string) *corev1.EnvVarSource {
			return &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: r.WorkerS3Secret}, Key: key,
			}}
		}
		// Ahead of the Evaluator's own env, which can still override them.
		c.Env = append([]corev1.EnvVar{
			{Name: "EVALSI_S3_ACCESS_KEY", ValueFrom: secret("access_key")},
			{Name: "EVALSI_S3_SECRET_KEY", ValueFrom: secret("secret_key")},
		}, c.Env...)
	}
}

// scaledObject scales the Deployment on the backlog of the pools'
// consumers, one trigger per pool.
func (r *EvaluatorReconciler) scaledObject(ctx context.Context, ev *v1.Evaluator, name string, pools []string) metav1.Condition {
	if _, err := r.RESTMapper().RESTMapping(scaledObjectGVK.GroupKind(), scaledObjectGVK.Version); err != nil {
		return metav1.Condition{Type: "Autoscaling", Status: metav1.ConditionFalse, Reason: "KEDAMissing", Message: "KEDA's ScaledObject is not installed"}
	}
	if r.NATSMonitoringEndpoint == "" {
		return metav1.Condition{Type: "Autoscaling", Status: metav1.ConditionFalse, Reason: "NoEndpoint", Message: "the operator has no NATS monitoring endpoint"}
	}
	a := ev.Spec.Autoscaling
	lag := a.LagThreshold
	if lag <= 0 {
		lag = 10
	}
	var triggers []any
	for _, pool := range pools {
		triggers = append(triggers, map[string]any{
			"type": "nats-jetstream",
			"metadata": map[string]any{
				"natsServerMonitoringEndpoint": r.NATSMonitoringEndpoint,
				"account":                      "$G",
				"stream":                       workStream,
				"consumer":                     consumerPrefix + pool,
				"lagThreshold":                 fmt.Sprint(lag),
				"activationLagThreshold":       "0",
			},
		})
	}
	so := &unstructured.Unstructured{}
	so.SetGroupVersionKind(scaledObjectGVK)
	so.SetName(name)
	so.SetNamespace(ev.Namespace)
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, so, func() error {
		so.Object["spec"] = map[string]any{
			"scaleTargetRef":  map[string]any{"name": name},
			"minReplicaCount": int64(a.MinReplicas),
			"maxReplicaCount": int64(a.MaxReplicas),
			"triggers":        triggers,
		}
		return controllerutil.SetControllerReference(ev, so, r.Scheme())
	})
	if err != nil && !apierrors.IsNotFound(err) {
		return metav1.Condition{Type: "Autoscaling", Status: metav1.ConditionFalse, Reason: "Error", Message: err.Error()}
	}
	return metav1.Condition{Type: "Autoscaling", Status: metav1.ConditionTrue, Reason: "KEDA", Message: fmt.Sprintf("%d to %d replicas on the backlog of %s", a.MinReplicas, a.MaxReplicas, strings.Join(pools, ", "))}
}

func (r *EvaluatorReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&v1.Evaluator{}).Owns(&appsv1.Deployment{}).Named("evaluator").Complete(r)
}
