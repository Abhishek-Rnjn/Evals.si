package helm

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/abhishek-rnjn/evals.si/operator/bootstrap"
)

var bootstrapValues = []string{
	"bootstrap.enabled=true",
	"bootstrap.projects[0].name=studio",
	"bootstrap.apiKeys[0].name=studio-ingest", "bootstrap.apiKeys[0].roles.studio[0]=ingest", "bootstrap.apiKeys[0].secret.name=studio-key",
	"bootstrap.webhooks[0].name=studio", "bootstrap.webhooks[0].project=studio", "bootstrap.webhooks[0].url=http://studio.studio.svc/hooks",
	"bootstrap.webhooks[0].events[0]=run.finished", "bootstrap.webhooks[0].secret.name=studio-hook",
	"bootstrap.policies[0].name=quality", "bootstrap.policies[0].project=studio",
	"bootstrap.credentialGrants[0].env=ANTHROPIC_API_KEY", "bootstrap.credentialGrants[0].projects[0]=studio",
	"bootstrap.credentialGrants[0].hosts[0]=api.anthropic.com",
	"bootstrap.tolerations[0].key=dedicated", "bootstrap.tolerations[0].operator=Exists",
	"bootstrap.nodeSelector.pool=infra", `bootstrap.podLabels.istio\.io/dataplane-mode=none`,
	"global.imageRegistry=mirror.example.com/team",
}

func TestBootstrapJob(t *testing.T) {
	objs := render(t, "evalsi", bootstrapValues...)

	// Everything runs after the release and is cleaned up once it has succeeded.
	for _, o := range []object{
		find(t, objs, "Job", "evalsi-bootstrap"), find(t, objs, "ConfigMap", "evalsi-bootstrap"),
		find(t, objs, "ServiceAccount", "evalsi-bootstrap"), find(t, objs, "Role", "evalsi-bootstrap"),
		find(t, objs, "RoleBinding", "evalsi-bootstrap"),
	} {
		a := o.Metadata.Annotations
		if a["helm.sh/hook"] != "post-install,post-upgrade" || !strings.Contains(a["helm.sh/hook-delete-policy"], "before-hook-creation") {
			t.Errorf("%s %s is not a post-install and post-upgrade hook that is replaced each time: %v", o.Kind, o.Metadata.Name, a)
		}
	}

	// The file it reads is one the bootstrap command accepts.
	raw := find(t, objs, "ConfigMap", "evalsi-bootstrap").Data["bootstrap.json"]
	cfg, err := bootstrap.Load([]byte(raw))
	if err != nil {
		t.Fatalf("%v\n%s", err, raw)
	}
	if len(cfg.Projects) != 1 || cfg.APIKeys[0].Secret.Name != "studio-key" || cfg.Webhooks[0].Events[0] != "run.finished" || len(cfg.Policies) != 1 {
		t.Errorf("config %+v", cfg)
	}

	// It can create Secrets, and read and replace only those it keeps keys in.
	var named []string
	for _, r := range find(t, objs, "Role", "evalsi-bootstrap").Rules {
		if len(r.ResourceNames) > 0 {
			named = append(named, r.ResourceNames...)
			if strings.Join(r.Verbs, ",") != "get,update" {
				t.Errorf("verbs on named Secrets: %v", r.Verbs)
			}
		}
	}
	if strings.Join(named, ",") != "studio-key,studio-hook" {
		t.Errorf("the Role names Secrets %v", named)
	}

	// Its service account is an owner of the install, and the grants reach the server.
	server := load(t, find(t, objs, "ConfigMap", "evalsi"))
	owned := false
	for _, o := range server.RBAC.Owners {
		owned = owned || o == "user:kubernetes/system:serviceaccount:evalsi:evalsi-bootstrap"
	}
	if !owned {
		t.Errorf("owners %v", server.RBAC.Owners)
	}
	grants := server.Credentials.Grants
	if len(grants) != 1 || grants[0].Env != "ANTHROPIC_API_KEY" || grants[0].Hosts[0] != "api.anthropic.com" {
		t.Errorf("grants %+v", grants)
	}

	// Scheduling, labels and the registry are values, like every other pod.
	job := toYAML(t, find(t, objs, "Job", "evalsi-bootstrap").Spec)
	for _, want := range []string{"key: dedicated", "pool: infra", "istio.io/dataplane-mode: none", "image: mirror.example.com/team/"} {
		if !strings.Contains(job, want) {
			t.Errorf("the Job lacks %q:\n%s", want, job)
		}
	}
}

func TestBootstrapIsOffByDefaultAndNeedsKubernetesAuth(t *testing.T) {
	for _, o := range render(t, "evalsi") {
		if strings.Contains(o.Metadata.Name, "bootstrap") {
			t.Errorf("%s %s rendered by default", o.Kind, o.Metadata.Name)
		}
	}
	args := []string{"template", "t", charts + "/evalsi", "--kube-version", "1.34.0", "--set", "bootstrap.enabled=true", "--set", "auth.kubernetes.enabled=false"}
	out, err := exec.Command(helm(t), args...).CombinedOutput()
	if err == nil || !strings.Contains(string(out), "auth.kubernetes.enabled must be on") {
		t.Errorf("bootstrap without Kubernetes auth: %v\n%s", err, out)
	}
}

// A key the Job makes is useless if the server rejects API keys.
func TestBootstrapKeysEnableAPIKeyAuth(t *testing.T) {
	cfg := find(t, render(t, "evalsi", bootstrapValues...), "ConfigMap", "evalsi").Data["evalsi.yaml"]
	if !strings.Contains(cfg, "api_keys:") {
		t.Errorf("bootstrap.apiKeys is set but the server config has no auth.api_keys:\n%s", cfg)
	}
	cfg = find(t, render(t, "evalsi"), "ConfigMap", "evalsi").Data["evalsi.yaml"]
	if strings.Contains(cfg, "api_keys:") {
		t.Errorf("API keys are on without bootstrap keys:\n%s", cfg)
	}
}
