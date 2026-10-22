package helm

import (
	"strings"
	"testing"
)

// The webhook guard is on by default in the server; the chart passes the
// allow list, adds the hosts of the webhooks it bootstraps, and can turn the
// guard off.
func TestWebhookGuardValues(t *testing.T) {
	plain := find(t, render(t, "evalsi"), "ConfigMap", "evalsi").Data["evalsi.yaml"]
	if strings.Contains(plain, "webhooks:") {
		t.Errorf("a webhooks section without webhook values:\n%s", plain)
	}

	objs := render(t, "evalsi", append(bootstrapValues,
		"webhooks.allowHosts[0]=*.studio.svc.cluster.local",
		"bootstrap.webhooks[1].name=ops", "bootstrap.webhooks[1].url=https://Hooks.Example.com:8443/x",
		"bootstrap.webhooks[1].events[0]=run.finished")...)
	cfg := find(t, objs, "ConfigMap", "evalsi").Data["evalsi.yaml"]
	for _, want := range []string{"webhooks:", "- '*.studio.svc.cluster.local'", "- studio.studio.svc", "- hooks.example.com"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("config lacks %q:\n%s", want, cfg)
		}
	}
	if strings.Contains(cfg, "ssrf_guard") {
		t.Errorf("ssrf_guard written while on (the server's default):\n%s", cfg)
	}

	off := find(t, render(t, "evalsi", "webhooks.ssrfGuard=false"), "ConfigMap", "evalsi").Data["evalsi.yaml"]
	if !strings.Contains(off, "ssrf_guard: false") {
		t.Errorf("ssrfGuard=false not passed:\n%s", off)
	}
}
