package sandbox

import (
	"context"
	"fmt"
	"path"
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
	// The fields below widen what a service may be. The pod backend takes Ports
	// and Aliases; the rest are for the bubblewrap backend (see bwrap_services.go)
	// and make the pod backend refuse the spec rather than ignore them.
	Ports        []int             `json:"ports,omitempty"`
	ReadyCommand []string          `json:"ready_command,omitempty"`
	Aliases      []string          `json:"aliases,omitempty"`
	Files        map[string]string `json:"files,omitempty"`
	Setup        []string          `json:"setup,omitempty"`
	SetupNetwork string            `json:"setup_network,omitempty"`
	Oneshot      bool              `json:"oneshot,omitempty"`
	WorkdirMount string            `json:"workdir_mount,omitempty"`
	Volumes      []string          `json:"volumes,omitempty"`
	Workdir      string            `json:"workdir,omitempty"`
	// Run the image's ENTRYPOINT with Command as its arguments (its CMD when Command
	// is empty), as docker does, instead of Command being the whole argv.
	ImageEntrypoint bool `json:"image_entrypoint,omitempty"`
}

// bwrapOnly names the fields only the bubblewrap backend supports, for a
// backend that does not to refuse.
func (s ServiceSpec) bwrapOnly() string {
	switch {
	case len(s.ReadyCommand) > 0:
		return "ready_command"
	case len(s.Files) > 0:
		return "files"
	case len(s.Setup) > 0:
		return "setup"
	case s.SetupNetwork != "":
		return "setup_network"
	case s.Oneshot:
		return "oneshot"
	case s.WorkdirMount != "":
		return "workdir_mount"
	case len(s.Volumes) > 0:
		return "volumes"
	case s.Workdir != "":
		return "workdir"
	}
	return ""
}

func ServicesFromProto(items []*evalsiv1alpha1.EnvironmentService) []ServiceSpec {
	out := make([]ServiceSpec, 0, len(items))
	for _, s := range items {
		out = append(out, ServiceSpec{
			Name: s.GetName(), Image: s.GetImage(), Command: s.GetCommand(), Env: s.GetEnv(),
			Port: int(s.GetPort()), DependsOn: s.GetDependsOn(), MemoryMB: int(s.GetMemoryMb()), CPUMillis: int(s.GetCpuMillis()),
			Ports: intsFrom(s.GetPorts()), ReadyCommand: s.GetReadyCommand(), Aliases: s.GetAliases(), Files: s.GetFiles(),
			Setup: s.GetSetup(), SetupNetwork: s.GetSetupNetwork(), Oneshot: s.GetOneshot(), WorkdirMount: s.GetWorkdirMount(),
			Volumes: s.GetVolumes(), Workdir: s.GetWorkdir(), ImageEntrypoint: s.GetImageEntrypoint(),
		})
	}
	return out
}

func intsFrom(in []int32) []int {
	var out []int
	for _, v := range in {
		out = append(out, int(v))
	}
	return out
}

var aliasName = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,61}[a-z0-9])?$`)
var volumeName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,39}$`)

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
	names, ports, aliases := map[string]bool{}, map[int]bool{podAgentPort: true}, map[string]bool{}
	declared := map[string]bool{}
	for _, s := range services {
		declared[s.Name] = true
	}
	for _, s := range services {
		if !serviceName.MatchString(s.Name) || strings.HasSuffix(s.Name, "-") || s.Name == "localhost" || s.Name == "sandbox" || s.Name == "guest" || s.Name == "workdir" {
			return fmt.Errorf("invalid or reserved service name %q", s.Name)
		}
		if names[s.Name] {
			return fmt.Errorf("duplicate service name %q", s.Name)
		}
		names[s.Name] = true
		if (len(s.Command) == 0 && !s.ImageEntrypoint) || (len(s.Command) > 0 && s.Command[0] == "") {
			return fmt.Errorf("service %s requires a command", s.Name)
		}
		// Every port is unique within the sandbox. A service needs a readiness port on
		// the pod backend (which says so); on bubblewrap it may have none, or a
		// ready_command, or be one-shot.
		all := append([]int{}, s.Ports...)
		if s.Port != 0 {
			all = append([]int{s.Port}, all...)
		}
		for _, port := range all {
			if port < 1 || port > 65535 || ports[port] {
				return fmt.Errorf("service %s requires unique ports from 1 to 65535 (7070 is reserved)", s.Name)
			}
			ports[port] = true
		}
		for _, a := range s.Aliases {
			if !aliasName.MatchString(a) || a == "localhost" || aliases[a] || declared[a] {
				return fmt.Errorf("service %s has an invalid, reserved or duplicate alias %q", s.Name, a)
			}
			aliases[a] = true
		}
		if err := validateServiceExtras(&s); err != nil {
			return err
		}
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

// parseVolume splits "name:/path" or "name:/path:ro".
func parseVolume(v string) (name, dest string, ro bool, err error) {
	parts := strings.Split(v, ":")
	if len(parts) == 3 && parts[2] == "ro" {
		ro, parts = true, parts[:2]
	}
	if len(parts) != 2 || !volumeName.MatchString(parts[0]) {
		return "", "", false, fmt.Errorf("volume %q must be name:/path or name:/path:ro", v)
	}
	return parts[0], parts[1], ro, nil
}

func validateServiceExtras(s *ServiceSpec) error {
	absClean := func(what, p string) error {
		if !path.IsAbs(p) || path.Clean(p) != p || p == "/" {
			return fmt.Errorf("service %s: %s %q must be a clean absolute path below /", s.Name, what, p)
		}
		return nil
	}
	if s.Workdir != "" {
		if err := absClean("workdir", s.Workdir); err != nil {
			return err
		}
	}
	if s.WorkdirMount != "" {
		if err := absClean("workdir_mount", s.WorkdirMount); err != nil {
			return err
		}
	}
	for _, v := range s.Volumes {
		_, dest, _, err := parseVolume(v)
		if err != nil {
			return fmt.Errorf("service %s: %w", s.Name, err)
		}
		if err := absClean("volume path", dest); err != nil {
			return err
		}
	}
	for name := range s.Files {
		clean := path.Clean("/" + name)
		if name == "" || strings.ContainsRune(name, 0) || strings.Contains(name, "..") || clean == "/" {
			return fmt.Errorf("service %s: invalid file path %q", s.Name, name)
		}
	}
	for _, c := range append(append([]string{}, s.ReadyCommand...), s.Setup...) {
		if strings.ContainsRune(c, 0) {
			return fmt.Errorf("service %s has NUL in a command", s.Name)
		}
	}
	if len(s.ReadyCommand) > 0 && s.ReadyCommand[0] == "" {
		return fmt.Errorf("service %s: empty ready_command", s.Name)
	}
	switch s.SetupNetwork {
	case "", NetworkDeny, NetworkAllow:
	default:
		return fmt.Errorf("service %s: setup_network must be deny or allow", s.Name)
	}
	if s.Oneshot && len(s.ReadyCommand) > 0 {
		return fmt.Errorf("service %s: a one-shot service is ready when it exits 0, not by ready_command", s.Name)
	}
	return nil
}
