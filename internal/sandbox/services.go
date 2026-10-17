package sandbox

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

// ServiceSpec is a private sidecar in a task pod. Nothing is inherited from
// the agent's environment, files, volumes or credentials.
type ServiceSpec struct {
	Name      string            `json:"name"`
	Image     string            `json:"image"`
	Command   []string          `json:"command"`
	Env       map[string]string `json:"env,omitempty"`
	Port      int               `json:"port"`
	DependsOn []string          `json:"depends_on,omitempty"`
	MemoryMB  int               `json:"memory_mb"`
	CPUMillis int               `json:"cpu_millis"`
}

func ServicesFromProto(items []*evalsiv1alpha1.EnvironmentService) []ServiceSpec {
	out := make([]ServiceSpec, 0, len(items))
	for _, s := range items {
		out = append(out, ServiceSpec{
			Name: s.GetName(), Image: s.GetImage(), Command: s.GetCommand(), Env: s.GetEnv(),
			Port: int(s.GetPort()), DependsOn: s.GetDependsOn(), MemoryMB: int(s.GetMemoryMb()), CPUMillis: int(s.GetCpuMillis()),
		})
	}
	return out
}

var serviceName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)

func ValidateServices(services []ServiceSpec, lifetime time.Duration) error {
	if len(services) == 0 {
		if lifetime != 0 {
			return fmt.Errorf("service_lifetime requires services")
		}
		return nil
	}
	if len(services) > 8 {
		return fmt.Errorf("at most 8 services are supported")
	}
	if lifetime < time.Second || lifetime > 24*time.Hour {
		return fmt.Errorf("services require service_lifetime from 1s to 24h")
	}
	names, ports := map[string]bool{}, map[int]bool{podAgentPort: true}
	for _, s := range services {
		if !serviceName.MatchString(s.Name) || strings.HasSuffix(s.Name, "-") || s.Name == "localhost" || s.Name == "sandbox" || s.Name == "guest" || s.Name == "workdir" {
			return fmt.Errorf("invalid or reserved service name %q", s.Name)
		}
		if names[s.Name] {
			return fmt.Errorf("duplicate service name %q", s.Name)
		}
		names[s.Name] = true
		if strings.TrimSpace(s.Image) == "" || len(s.Command) == 0 || s.Command[0] == "" {
			return fmt.Errorf("service %s requires image and command", s.Name)
		}
		if s.Port < 1024 || s.Port > 65535 || ports[s.Port] {
			return fmt.Errorf("service %s requires a unique port from 1024 to 65535 (7070 is reserved)", s.Name)
		}
		ports[s.Port] = true
		if s.MemoryMB < 16 || s.MemoryMB > 65536 || s.CPUMillis < 1 || s.CPUMillis > 64000 {
			return fmt.Errorf("service %s requires memory_mb (16..65536) and cpu_millis (1..64000)", s.Name)
		}
		for k, v := range s.Env {
			if k == "" || strings.ContainsAny(k, "=\x00") || strings.ContainsRune(v, 0) {
				return fmt.Errorf("service %s has an invalid environment variable", s.Name)
			}
		}
		for _, arg := range s.Command {
			if strings.ContainsRune(arg, 0) {
				return fmt.Errorf("service %s has NUL in command", s.Name)
			}
		}
	}
	_, err := orderServices(services)
	return err
}

// Stable topological order. Native sidecar startup probes gate later init
// containers, so dependencies become ready before their dependents start.
func orderServices(services []ServiceSpec) ([]ServiceSpec, error) {
	byName := map[string]ServiceSpec{}
	for _, s := range services {
		byName[s.Name] = s
	}
	visiting, done := map[string]bool{}, map[string]bool{}
	var out []ServiceSpec
	var visit func(string) error
	visit = func(name string) error {
		if done[name] {
			return nil
		}
		if visiting[name] {
			return fmt.Errorf("service dependency cycle at %s", name)
		}
		s, ok := byName[name]
		if !ok {
			return fmt.Errorf("unknown service dependency %q", name)
		}
		visiting[name] = true
		for _, dep := range s.DependsOn {
			if err := visit(dep); err != nil {
				return err
			}
		}
		visiting[name] = false
		done[name] = true
		out = append(out, s)
		return nil
	}
	for _, s := range services {
		if err := visit(s.Name); err != nil {
			return nil, err
		}
	}
	return out, nil
}

type ServiceStatus struct {
	Name     string
	Ready    bool
	Restarts int32
	Logs     string
	Reason   string
}

// Services inspects sidecar health without exposing service environment values.
func (ss *Session) Services(ctx context.Context, logs bool) ([]ServiceStatus, error) {
	if b, ok := ss.b.(interface {
		services(context.Context, bool) ([]ServiceStatus, error)
	}); ok {
		return b.services(ctx, logs)
	}
	return nil, nil
}
