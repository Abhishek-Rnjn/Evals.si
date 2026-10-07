package v1alpha1

import (
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ProjectOf is the evalsi project of a namespaced resource: its
// evals.si/project label, else its namespace.
func ProjectOf(obj metav1.Object) string {
	if p := obj.GetLabels()[ProjectLabel]; p != "" {
		return p
	}
	return obj.GetNamespace()
}

// APILabels are the resource's labels that travel to the API (for access
// rules), without Kubernetes' own prefixed ones.
func APILabels(obj metav1.Object) map[string]string {
	out := map[string]string{}
	for k, v := range obj.GetLabels() {
		if !strings.Contains(k, "/") {
			out[k] = v
		}
	}
	return out
}
