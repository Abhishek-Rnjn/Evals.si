package controllers

import (
	"context"
	"fmt"
	"slices"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/yaml"

	"github.com/abhishek-rnjn/evals.si/internal/sandbox"
	v1 "github.com/abhishek-rnjn/evals.si/operator/api/v1alpha1"
)

// SandboxPort is where a sandbox pool serves the sandbox service.
const SandboxPort = 7443

// SandboxClassReconciler runs a sandbox pool per SandboxClass: a
// Deployment of `evalsid sandbox serve` behind a Service, over mutual TLS.
type SandboxClassReconciler struct {
	client.Client
	// Where pools run.
	Namespace string
	// The evalsi image (evalsid and evalsi-guest).
	Image string
	// A kubernetes.io/tls Secret with ca.crt (cert-manager's form): the
	// pool's certificate and the CA its clients' certificates come from.
	TLSSecret string
	// Clients allowed in (SPIFFE IDs or common names); default: any
	// certificate from the CA.
	AllowClients []string
	// The pool's service account; the pod rung needs one that may manage
	// pods in the sandbox namespace.
	ServiceAccount string
}

// +kubebuilder:rbac:groups=evals.si,resources=sandboxclasses,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=evals.si,resources=sandboxclasses/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=configmaps;services,verbs=get;list;watch;create;update;patch;delete

func (r *SandboxClassReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	sc := &v1.SandboxClass{}
	if err := r.Get(ctx, req.NamespacedName, sc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	name := "evalsi-sandbox-" + sc.Name
	labels := map[string]string{"app.kubernetes.io/name": "evalsi-sandboxd", "app.kubernetes.io/instance": sc.Name, "app.kubernetes.io/managed-by": "evalsi-operator"}
	cfg := r.sandboxConfig(sc)
	rendered, err := yaml.Marshal(map[string]any{"sandbox": cfg})
	if err != nil {
		return ctrl.Result{}, err
	}

	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Labels = labels
		cm.Data = map[string]string{"evalsi.yaml": string(rendered)}
		return controllerutil.SetControllerReference(sc, cm, r.Scheme())
	}); err != nil {
		return ctrl.Result{}, err
	}

	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		dep.Labels = labels
		dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
		dep.Spec.Replicas = ptr.To(ptr.Deref(sc.Spec.Replicas, 1))
		pod := &dep.Spec.Template
		pod.Labels = labels
		// A new config rolls the pool.
		pod.Annotations = map[string]string{"evals.si/config-hash": hash(rendered)}
		r.podSpec(sc, name, &pod.Spec)
		return controllerutil.SetControllerReference(sc, dep, r.Scheme())
	}); err != nil {
		return ctrl.Result{}, err
	}

	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = labels
		svc.Spec.Selector = labels
		svc.Spec.Ports = []corev1.ServicePort{{Name: "sandbox", Port: SandboxPort, TargetPort: intstr.FromInt32(SandboxPort)}}
		return controllerutil.SetControllerReference(sc, svc, r.Scheme())
	}); err != nil {
		return ctrl.Result{}, err
	}

	sc.Status.Address = fmt.Sprintf("tls://%s.%s.svc:%d", name, r.Namespace, SandboxPort)
	sc.Status.ReadyReplicas = dep.Status.ReadyReplicas
	sc.Status.ObservedGeneration = sc.Generation
	ready := metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, Reason: "Starting", ObservedGeneration: sc.Generation}
	if dep.Status.ReadyReplicas > 0 {
		ready.Status, ready.Reason = metav1.ConditionTrue, "Serving"
	}
	meta.SetStatusCondition(&sc.Status.Conditions, ready)
	return ctrl.Result{}, r.Status().Update(ctx, sc)
}

func (r *SandboxClassReconciler) ladder(sc *v1.SandboxClass) []string {
	if len(sc.Spec.Ladder) > 0 {
		return sc.Spec.Ladder
	}
	// Nodes seldom have KVM; Firecracker is opted into.
	return []string{"bwrap", "landlock"}
}

func (r *SandboxClassReconciler) sandboxConfig(sc *v1.SandboxClass) sandbox.Config {
	s := sc.Spec
	cfg := sandbox.Config{
		Ladder: r.ladder(sc), MinIsolation: s.MinIsolation, MaxSandboxes: int(s.MaxSandboxes),
		WorkDir: "/var/lib/evalsi/sandboxes", CacheDir: "/var/lib/evalsi/cache",
	}
	if p := s.Pod; p != nil || slices.Contains(cfg.Ladder, "pod") {
		if p == nil {
			p = &v1.PodRung{}
		}
		cfg.Pod = &sandbox.PodConfig{
			Namespace: r.Namespace, GuestImage: r.Image, DefaultImage: p.DefaultImage,
			RuntimeClassName: p.RuntimeClassName, Level: p.Level, RunAsUser: p.RunAsUser, Capabilities: p.Capabilities, CPU: p.CPU,
			NodeSelector: p.NodeSelector, NetworkPolicyEnforced: p.NetworkPolicyEnforced,
			Labels: map[string]string{"evals.si/sandbox-class": sc.Name},
		}
	}
	if f := s.Firecracker; f != nil {
		cfg.Firecracker = &sandbox.FirecrackerConfig{Kernel: f.Kernel, WarmPool: int(f.WarmPool), VCPUs: int(f.VCPUs), MemoryMB: int(f.MemoryMB)}
	}
	return cfg
}

func (r *SandboxClassReconciler) podSpec(sc *v1.SandboxClass, name string, spec *corev1.PodSpec) {
	ladder := r.ladder(sc)
	tls := "/etc/evalsi/tls"
	args := []string{
		"sandbox", "serve", "--config", configMount + "/evalsi.yaml",
		"--listen", fmt.Sprintf("tcp://0.0.0.0:%d", SandboxPort),
		"--tls-cert", tls + "/tls.crt", "--tls-key", tls + "/tls.key", "--client-ca", tls + "/ca.crt",
	}
	for _, id := range r.AllowClients {
		args = append(args, "--allow-client", id)
	}
	c := corev1.Container{
		Name: "sandboxd", Image: r.Image, Command: []string{"evalsid"}, Args: args,
		Ports:     []corev1.ContainerPort{{Name: "sandbox", ContainerPort: SandboxPort}},
		Resources: sc.Spec.Resources,
		Env: []corev1.EnvVar{{Name: "POD_IP", ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"},
		}}},
		ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
			TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(SandboxPort)},
		}, PeriodSeconds: 5},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "config", MountPath: configMount, ReadOnly: true},
			{Name: "tls", MountPath: tls, ReadOnly: true},
			{Name: "state", MountPath: "/var/lib/evalsi"},
			{Name: "tmp", MountPath: "/tmp"},
		},
	}
	spec.Volumes = []corev1.Volume{
		{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: name}}}},
		{Name: "tls", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: r.TLSSecret}}},
		{Name: "state", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}
	spec.HostUsers = nil
	spec.SecurityContext = nil
	// bwrap needs user and mount namespaces; Landlock alone and the pod rung
	// need no privilege (Pod Security "restricted").
	local := slices.Contains(ladder, "bwrap")
	switch {
	case slices.Contains(ladder, "firecracker") || (local && sc.Spec.PodSecurity == "privileged"):
		spec.SecurityContext = &corev1.PodSecurityContext{RunAsUser: ptr.To(int64(0))}
		c.SecurityContext = &corev1.SecurityContext{Privileged: ptr.To(true)}
		if slices.Contains(ladder, "firecracker") {
			c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "kvm", MountPath: "/dev/kvm"})
			spec.Volumes = append(spec.Volumes, corev1.Volume{Name: "kvm", VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: "/dev/kvm", Type: ptr.To(corev1.HostPathCharDev)},
			}})
		}
	case local:
		// Root in a user namespace: no privilege on the node, but enough to
		// make the namespaces bwrap needs and mount its own /proc.
		spec.HostUsers = ptr.To(false)
		spec.SecurityContext = &corev1.PodSecurityContext{RunAsUser: ptr.To(int64(0))}
		c.SecurityContext = &corev1.SecurityContext{
			ProcMount:       ptr.To(corev1.UnmaskedProcMount),
			SeccompProfile:  &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined},
			AppArmorProfile: &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeUnconfined},
		}
	default:
		// The pod rung only talks to the API server and its sandboxes;
		// Landlock confines its sandboxes from inside this pod.
		spec.SecurityContext = &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}}
		c.SecurityContext = &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr.To(false), ReadOnlyRootFilesystem: ptr.To(true),
			Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		}
	}
	spec.ServiceAccountName = r.ServiceAccount
	spec.AutomountServiceAccountToken = ptr.To(slices.Contains(ladder, "pod"))
	spec.NodeSelector = sc.Spec.NodeSelector
	spec.Tolerations = sc.Spec.Tolerations
	spec.Containers = []corev1.Container{c}
}

func (r *SandboxClassReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&v1.SandboxClass{}).
		Owns(&appsv1.Deployment{}).Owns(&corev1.Service{}).Owns(&corev1.ConfigMap{}).
		Named("sandboxclass").Complete(r)
}
