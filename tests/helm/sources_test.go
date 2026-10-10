package helm

import (
	"strings"
	"testing"
)

// Trace sources (decision 0016): mounted credential Secrets, the sources
// config, the opt-in egress policy and the lag alerts.
func TestSourcesValues(t *testing.T) {
	objs := render(t, "evalsi",
		"sources.secrets[0]=mlflow-token", "sources.secrets[1]=langfuse.keys", "sources.allowHosts[0]=mlflow.studio.svc.cluster.local",
		"sources.maxRecordsPerSecond=50",
	)
	cfg := find(t, objs, "ConfigMap", "evalsi").Data["evalsi.yaml"]
	for _, want := range []string{"dir: /etc/evalsi/sources", "- mlflow.studio.svc.cluster.local", "max_records_per_second: 50"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("config lacks %q:\n%s", want, cfg)
		}
	}
	dep := podSpec(t, find(t, objs, "Deployment", "evalsi"))
	for _, want := range []string{
		"mountPath: /etc/evalsi/sources/mlflow-token", "secretName: mlflow-token",
		"mountPath: /etc/evalsi/sources/langfuse.keys", "secretName: langfuse.keys",
		// Volume names are DNS labels: by index, as a Secret name may have dots.
		"name: source-secret-0", "name: source-secret-1",
	} {
		if !strings.Contains(dep, want) {
			t.Errorf("server pod lacks %q", want)
		}
	}
	// Nothing about sources unless asked.
	plain := render(t, "evalsi")
	if strings.Contains(find(t, plain, "ConfigMap", "evalsi").Data["evalsi.yaml"], "sources:") {
		t.Error("a sources section without sources values")
	}
	for _, o := range plain {
		if o.Kind == "PrometheusRule" || (o.Kind == "NetworkPolicy" && strings.HasSuffix(o.Metadata.Name, "-egress")) {
			t.Errorf("%s %s rendered by default", o.Kind, o.Metadata.Name)
		}
	}
}

func TestServerEgressPolicy(t *testing.T) {
	objs := render(t, "evalsi", "networkPolicy.egress.enabled=true",
		"networkPolicy.egress.extra[0].to[0].ipBlock.cidr=203.0.113.0/24",
		"networkPolicy.egress.extra[0].ports[0].port=443")
	np := toYAML(t, find(t, objs, "NetworkPolicy", "evalsi-egress").Spec)
	for _, want := range []string{"app.kubernetes.io/name: evalsi", "- Egress", "port: 53", "podSelector: {}", "cidr: 203.0.113.0/24", "port: 443"} {
		if !strings.Contains(np, want) {
			t.Errorf("egress policy lacks %q:\n%s", want, np)
		}
	}
	// It selects only the evalsid pods, under another release's name too.
	other := renderAs(t, "team-a", "evalsi", "networkPolicy.egress.enabled=true")
	if np := toYAML(t, find(t, other, "NetworkPolicy", "team-a-evalsi-egress").Spec); !strings.Contains(np, "app.kubernetes.io/name: team-a-evalsi") {
		t.Errorf("selector: %s", np)
	}
}

func TestPrometheusRule(t *testing.T) {
	objs := render(t, "evalsi", "prometheusRule.enabled=true", "prometheusRule.labels.release=kube-prometheus", "prometheusRule.sourceLagSeconds=300")
	rule := find(t, objs, "PrometheusRule", "evalsi")
	spec := toYAML(t, rule.Spec)
	for _, want := range []string{"evalsi_source_lag_seconds > 300", "evalsi_source_failing == 1", "$labels.source"} {
		if !strings.Contains(spec, want) {
			t.Errorf("rule lacks %q:\n%s", want, spec)
		}
	}
	if rule.Metadata.Labels["release"] != "kube-prometheus" {
		t.Errorf("labels %v", rule.Metadata.Labels)
	}
}

// The operator may manage TraceSources, only namespace admins may write them,
// and admission checks them.
func TestTraceSourceRBACAndAdmission(t *testing.T) {
	has := func(rules []rule, resource string, verbs ...string) bool {
		for _, r := range rules {
			if !contains(r.Resources, resource) {
				continue
			}
			ok := true
			for _, v := range verbs {
				ok = ok && contains(r.Verbs, v)
			}
			if ok {
				return true
			}
		}
		return false
	}
	role := find(t, render(t, "evalsi"), "Role", "evalsi-operator")
	if !has(role.Rules, "tracesources", "watch", "update") || !has(role.Rules, "tracesources/status", "update") || !has(role.Rules, "tracesources/finalizers", "update") {
		t.Errorf("namespaced operator role: %+v", role.Rules)
	}
	crds := render(t, "evalsi-crds", "operator.allNamespaces=true")
	if r := find(t, crds, "ClusterRole", "evalsi-operator"); !has(r.Rules, "tracesources", "watch") || !has(r.Rules, "tracesources/status", "update") {
		t.Errorf("cluster operator role: %+v", r.Rules)
	}
	if r := find(t, crds, "ClusterRole", "evalsi-admin"); r.Metadata.Labels["rbac.authorization.k8s.io/aggregate-to-admin"] != "true" || !has(r.Rules, "tracesources", "create", "delete") {
		t.Errorf("admin role: %+v %v", r.Rules, r.Metadata.Labels)
	}
	if r := find(t, crds, "ClusterRole", "evalsi-edit"); has(r.Rules, "tracesources", "create") {
		t.Error("namespace editors may create trace sources; only admins should")
	}
	if r := find(t, crds, "ClusterRole", "evalsi-view"); !has(r.Rules, "tracesources", "get", "list") {
		t.Errorf("view role: %+v", r.Rules)
	}
	for _, kind := range []string{"MutatingWebhookConfiguration", "ValidatingWebhookConfiguration"} {
		for _, o := range find2(crds, kind) {
			if !strings.Contains(toYAML(t, o.Webhooks), "tracesources") {
				t.Errorf("%s %s does not cover tracesources", kind, o.Metadata.Name)
			}
		}
	}
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
