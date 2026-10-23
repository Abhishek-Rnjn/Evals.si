package sandbox

import (
	"context"
	"strings"
	"testing"
)

func TestBwrapServiceFileMountsAgentVolumesAndLowPorts(t *testing.T) {
	// A file bound into a volume that the agent also sees, a read-only volume for
	// the agent, a service with no readiness port, and a port below 1024.
	writer := ServiceSpec{
		Name: "logger", MemoryMB: 128, CPUMillis: 100, Volumes: []string{"logs:/data"},
		Command: []string{"sh", "-c", "echo line > /data/log.txt; exec sleep 600"},
	}
	web := serving("web", 80, "/data")
	web.Volumes = []string{"logs:/data"}
	web.WorkdirMount = "/agent"
	web.Files = map[string]string{"/agent/server_secret.py": "print('hidden')\n"}
	web.DependsOn = []string{"logger"}
	sp := servicesSpec(nil, web, writer)
	sp.Volumes = []string{"logs:/workspace/logs:ro"}
	_, sess := bwrapServices(t, sp)
	if out, res := svcExec(t, sess, fetch+"http://web:80/log.txt"); strings.TrimSpace(out) != "line" {
		t.Fatalf("port 80 or the shared volume: %q %s", out, res.Error)
	}
	if out, _ := svcExec(t, sess, "cat logs/log.txt"); strings.TrimSpace(out) != "line" {
		t.Fatalf("the agent's view of the volume: %q", out)
	}
	if _, res := svcExec(t, sess, "echo x > logs/new.txt"); res.ExitCode == 0 {
		t.Fatal("wrote to a read-only volume")
	}
	// The file bound over the mounted workdir is in the service, empty in the agent's view.
	if out, _ := svcExec(t, sess, "cat server_secret.py | wc -c"); strings.TrimSpace(out) != "0" {
		t.Fatalf("the bound file leaked into the workdir: %q", out)
	}
	st, _ := sess.Services(context.Background(), false)
	for _, s := range st {
		if s.Name == "logger" && !s.Ready {
			t.Fatalf("a service with no port is ready once up: %+v", s)
		}
	}
}

func TestImageEntrypointIsResolvedFromTheImage(t *testing.T) {
	g := &serviceGroup{b: &hostBackend{sb: &Sandbox{images: newImageStore(t.TempDir())}}}
	g.b.sb.images.allowLocal = true
	root := t.TempDir()
	s := ServiceSpec{Name: "x", Image: "dir:" + root, ImageEntrypoint: true}
	if _, err := g.command(context.Background(), s); err == nil || !strings.Contains(err.Error(), "no entrypoint or command") {
		t.Fatalf("an image with neither: %v", err)
	}
	s.Command = []string{"serve"}
	s.ImageEntrypoint = false
	if got, err := g.command(context.Background(), s); err != nil || len(got) != 1 || got[0] != "serve" {
		t.Fatalf("explicit argv: %v %v", got, err)
	}
}
