package v1alpha1

import (
	"fmt"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ProjectPolicy decides which evalsi project a namespaced resource may name.
// The operator calls evalsid with its own credential (an owner), so the
// evals.si/project label must not let whoever can create a resource in one
// namespace act in any project. A resource's project is its namespace, or
// the label when the label names the namespace itself, the resource is in
// Home (the operator's own namespace, whose editors already hold the
// release's Secrets), or Namespaces maps the project to the resource's
// namespace ("*" for every namespace).
//
// +kubebuilder:object:generate=false
type ProjectPolicy struct {
	Home       string
	Namespaces map[string][]string
}

// Project returns the resource's project, or why its label is refused.
func (p ProjectPolicy) Project(obj metav1.Object) (string, error) {
	ns := obj.GetNamespace()
	project := obj.GetLabels()[ProjectLabel]
	switch {
	case project == "" || project == ns:
		return ns, nil
	case p.Home != "" && ns == p.Home:
		return project, nil
	case slices.Contains(p.Namespaces[project], ns) || slices.Contains(p.Namespaces[project], "*"):
		return project, nil
	}
	return "", fmt.Errorf("%s %q: resources in namespace %q may not use project %q; "+
		"map the project to the namespace in the operator's project namespaces (the chart's operator.projectNamespaces)",
		ProjectLabel, project, ns, project)
}

// ParseProjectNamespaces parses "project=ns1,ns2" mappings.
func ParseProjectNamespaces(values []string) (map[string][]string, error) {
	out := map[string][]string{}
	for _, v := range values {
		project, list, ok := strings.Cut(v, "=")
		if !ok || project == "" || list == "" {
			return nil, fmt.Errorf("project namespaces %q: use project=namespace[,namespace...]", v)
		}
		for _, ns := range strings.Split(list, ",") {
			if ns = strings.TrimSpace(ns); ns != "" {
				out[project] = append(out[project], ns)
			}
		}
	}
	return out, nil
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
