package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Annotations and labels the operator and its webhook manage.
const (
	// Who created the resource, from the admission request (the webhook
	// sets it on create and keeps it from changing).
	CreatedByAnnotation = "evals.si/created-by"
	// The project a resource belongs to; default: its namespace.
	ProjectLabel = "evals.si/project"
	Finalizer    = "evals.si/finalizer"
)

// EvalRun is one offline run. Its spec is the run spec, exactly as in a run
// YAML for `evalsi run -f`, so the same file applies with kubectl. The
// operator creates the run through the evalsid API and mirrors its state in
// status. The spec cannot change after creation; apply a new EvalRun.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=evrun
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Run",type=string,JSONPath=`.status.runId`
// +kubebuilder:printcolumn:name="Done",type=integer,JSONPath=`.status.progress.done`
// +kubebuilder:printcolumn:name="Total",type=integer,JSONPath=`.status.progress.total`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type EvalRun struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// The run spec (target or agent, dataset, evaluators, trials, gates,
	// budget, harness, environment), validated by the webhook.
	// +kubebuilder:validation:Type=object
	// +kubebuilder:pruning:PreserveUnknownFields
	Spec   runtime.RawExtension `json:"spec"`
	Status EvalRunStatus        `json:"status,omitempty"`
}

// EvalRunStatus mirrors the run.
type EvalRunStatus struct {
	// Pending, Running, Succeeded, Failed (a gate failed), Error, Cancelled,
	// or Invalid (the API refused the spec).
	Phase   string `json:"phase,omitempty"`
	RunID   string `json:"runId,omitempty"`
	Project string `json:"project,omitempty"`
	Message string `json:"message,omitempty"`
	// +optional
	Progress   *Progress       `json:"progress,omitempty"`
	Summaries  []MetricSummary `json:"summaries,omitempty"`
	Gates      []GateResult    `json:"gates,omitempty"`
	StartedAt  *metav1.Time    `json:"startedAt,omitempty"`
	FinishedAt *metav1.Time    `json:"finishedAt,omitempty"`
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

type Progress struct {
	Done  int64 `json:"done"`
	Total int64 `json:"total"`
}

// MetricSummary is one metric's aggregate. Numbers are strings, as
// Kubernetes APIs avoid floats.
type MetricSummary struct {
	Metric string `json:"metric"`
	N      int64  `json:"n"`
	Mean   string `json:"mean,omitempty"`
	CILow  string `json:"ciLow,omitempty"`
	CIHigh string `json:"ciHigh,omitempty"`
	// Records left out of n: skipped (not applicable) or errored
	// (infrastructure errors, which are retried and never scored).
	Skipped int64 `json:"skipped,omitempty"`
	Errors  int64 `json:"errors,omitempty"`
}

type GateResult struct {
	Metric string `json:"metric"`
	Passed bool   `json:"passed"`
	Reason string `json:"reason,omitempty"`
}

// +kubebuilder:object:root=true
type EvalRunList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EvalRun `json:"items"`
}

// OnlineEvalPolicy evaluates live traces. Its spec is the policy (selector,
// sampling, stages, alerts, promotion); the API name is namespace.name and
// its project is the resource's.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=evpolicy
// +kubebuilder:printcolumn:name="Synced",type=string,JSONPath=`.status.conditions[?(@.type=="Synced")].status`
// +kubebuilder:printcolumn:name="Seen",type=integer,JSONPath=`.status.tracesSeen`
// +kubebuilder:printcolumn:name="Evaluated",type=integer,JSONPath=`.status.tracesEvaluated`
type OnlineEvalPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +kubebuilder:validation:Type=object
	// +kubebuilder:pruning:PreserveUnknownFields
	Spec   runtime.RawExtension   `json:"spec"`
	Status OnlineEvalPolicyStatus `json:"status,omitempty"`
}

type OnlineEvalPolicyStatus struct {
	// The policy's name in the API.
	Name               string `json:"name,omitempty"`
	ObservedGeneration int64  `json:"observedGeneration,omitempty"`
	TracesSeen         int64  `json:"tracesSeen,omitempty"`
	TracesEvaluated    int64  `json:"tracesEvaluated,omitempty"`
	TracesPromoted     int64  `json:"tracesPromoted,omitempty"`
	EvaluationErrors   int64  `json:"evaluationErrors,omitempty"`
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
type OnlineEvalPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OnlineEvalPolicy `json:"items"`
}

// Evaluator registers an evaluator plugin: an image with evalsid, the
// Python worker and the plugin's packages, run as a worker Deployment that
// serves pools from the work queues.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Image",type=string,JSONPath=`.spec.image`
// +kubebuilder:printcolumn:name="Pools",type=string,JSONPath=`.spec.pools`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
type Evaluator struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   EvaluatorSpec   `json:"spec"`
	Status EvaluatorStatus `json:"status,omitempty"`
}

type EvaluatorSpec struct {
	// The worker image.
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`
	// Pools it serves; default cpu.
	// +optional
	Pools []string `json:"pools,omitempty"`
	// Replicas without autoscaling; default 1.
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`
	// KEDA scaling on the pools' queue backlog (needs KEDA installed).
	// +optional
	Autoscaling *Autoscaling `json:"autoscaling,omitempty"`
	// Tasks per pool at once in each replica; default 4.
	// +optional
	Concurrency int32 `json:"concurrency,omitempty"`
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`
	// A ConfigMap holding the worker's evalsi.yaml (cluster, judges,
	// sandbox service); default: the operator's worker config.
	// +optional
	ConfigMap string `json:"configMap,omitempty"`
	// +optional
	ServiceAccountName string `json:"serviceAccountName,omitempty"`
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
}

type Autoscaling struct {
	MinReplicas int32 `json:"minReplicas"`
	MaxReplicas int32 `json:"maxReplicas"`
	// Backlog per replica; default 10.
	// +optional
	LagThreshold int64 `json:"lagThreshold,omitempty"`
}

type EvaluatorStatus struct {
	ReadyReplicas      int32 `json:"readyReplicas,omitempty"`
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
type EvaluatorList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Evaluator `json:"items"`
}

// SandboxClass is like a StorageClass for sandboxes: the isolation ladder,
// the minimum level, the rungs' settings and the pool's size. The operator
// runs a sandbox pool for it (evalsi-sandbox-<name>: a Deployment serving
// the sandbox service over mutual TLS, and its Service).
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Min",type=string,JSONPath=`.spec.minIsolation`
// +kubebuilder:printcolumn:name="Ladder",type=string,JSONPath=`.spec.ladder`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
type SandboxClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SandboxClassSpec   `json:"spec"`
	Status SandboxClassStatus `json:"status,omitempty"`
}

type SandboxClassSpec struct {
	// Rungs, strongest first: firecracker, bwrap, landlock, pod.
	// +optional
	Ladder []string `json:"ladder,omitempty"`
	// none, confined, namespaced, kernel or vm.
	// +optional
	MinIsolation string `json:"minIsolation,omitempty"`
	// Pool replicas; default 1.
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`
	// Sandboxes at once per replica.
	// +optional
	MaxSandboxes int32 `json:"maxSandboxes,omitempty"`
	// How the pool's pods get the kernel features bwrap and landlock need
	// (user and mount namespaces, their own /proc): userns runs them in a
	// user namespace with an unmasked /proc and no seccomp or AppArmor
	// profile; privileged runs them privileged, for nodes without user
	// namespaces. The Firecracker rung is always privileged (it needs
	// /dev/kvm); the pod rung needs neither.
	// +kubebuilder:validation:Enum=userns;privileged
	// +optional
	PodSecurity string `json:"podSecurity,omitempty"`
	// +optional
	Pod *PodRung `json:"pod,omitempty"`
	// +optional
	Firecracker *FirecrackerRung `json:"firecracker,omitempty"`
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
}

type PodRung struct {
	// +optional
	RuntimeClassName string `json:"runtimeClassName,omitempty"`
	// The level the runtime class gives (kernel for gVisor, vm for Kata).
	// +optional
	Level string `json:"level,omitempty"`
	// +optional
	DefaultImage string `json:"defaultImage,omitempty"`
	// +optional
	RunAsUser *int64 `json:"runAsUser,omitempty"`
	// +optional
	CPU string `json:"cpu,omitempty"`
	// +optional
	NetworkPolicyEnforced bool `json:"networkPolicyEnforced,omitempty"`
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
}

type FirecrackerRung struct {
	// Path of the guest kernel on the node.
	Kernel string `json:"kernel"`
	// +optional
	WarmPool int32 `json:"warmPool,omitempty"`
	// +optional
	VCPUs int32 `json:"vcpus,omitempty"`
	// +optional
	MemoryMB int32 `json:"memoryMB,omitempty"`
}

type SandboxClassStatus struct {
	ReadyReplicas      int32 `json:"readyReplicas,omitempty"`
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// The pool's Service, as workers address it (tls://...).
	Address string `json:"address,omitempty"`
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
type SandboxClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SandboxClass `json:"items"`
}

func init() {
	SchemeBuilder.Register(
		&EvalRun{}, &EvalRunList{},
		&OnlineEvalPolicy{}, &OnlineEvalPolicyList{},
		&Evaluator{}, &EvaluatorList{},
		&SandboxClass{}, &SandboxClassList{},
	)
}
