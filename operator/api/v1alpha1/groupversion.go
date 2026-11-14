// Package v1alpha1 holds the evalsi.dev/v1alpha1 custom resources: EvalRun,
// OnlineEvalPolicy, Evaluator and SandboxClass.
//
// +kubebuilder:object:generate=true
// +groupName=evalsi.dev
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	GroupVersion  = schema.GroupVersion{Group: "evalsi.dev", Version: "v1alpha1"}
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}
	AddToScheme   = SchemeBuilder.AddToScheme
)
