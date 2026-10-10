package runs

import (
	"os"
	"path/filepath"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/datasets"
)

// validateAgent checks an agent run's spec: the agent or the built-in
// agent's model, the harness, the environment, and anything the worker
// itself would execute, which must be trusted in the server config.
func (m *Manager) validateAgent(spec *evalsiv1alpha1.RunSpec) error {
	agent := spec.GetTarget().GetAgent()
	builtin := spec.GetHarness().GetExternal() == nil
	replay := spec.GetHarness().GetBuiltin().GetRecording().GetMode() == "replay"
	env := spec.GetEnvironment()
	switch {
	case agent.GetA2A() != nil:
		if agent.GetA2A().GetUrl() == "" {
			return invalid("spec.target.agent.a2a.url is required")
		}
	case agent.GetMcp() != nil:
		if agent.GetMcp().GetUrl() == "" {
			return invalid("spec.target.agent.mcp.url is required")
		}
	case agent.GetResponses() != nil:
		if r := agent.GetResponses(); r.GetBaseUrl() == "" || r.GetModel() == "" {
			return invalid("spec.target.agent.responses needs base_url and model")
		}
	case agent.GetHttp() != nil:
		if h := agent.GetHttp(); h.GetUrl() == "" || h.GetOutputPath() == "" {
			return invalid("spec.target.agent.http needs url and output_path")
		}
	case agent.GetCli() != nil:
		// A CLI agent runs in the task's sandbox; the environment may come
		// from the records (metadata.environment), so it is checked per task.
		if len(agent.GetCli().GetCommand()) == 0 {
			return invalid("spec.target.agent.cli.command is required")
		}
	case agent != nil:
		return invalid("spec.target.agent needs one of a2a, mcp, responses, http or cli")
	case builtin && !replay:
		t := spec.GetTarget()
		switch {
		case t == nil || t.GetModel() == "":
			return invalid("the built-in agent needs spec.target with a connector and model (or an agent under spec.target.agent)")
		case t.GetConnector() != "openai-compatible" && t.GetConnector() != "anthropic":
			return invalid("spec.target.connector must be openai-compatible or anthropic")
		case t.GetConnector() == "openai-compatible" && t.GetBaseUrl() == "":
			return invalid("spec.target.base_url is required for openai-compatible")
		}
	}
	if ext := spec.GetHarness().GetExternal(); ext != nil {
		switch {
		case ext.GetPython() != "":
			if err := m.trustPython(ext.GetPython(), "spec.harness.external.python"); err != nil {
				return err
			}
		case ext.GetCommand() != nil:
			if err := m.trustCommand(ext.GetCommand().GetArgv(), "spec.harness.external.command"); err != nil {
				return err
			}
		case ext.GetAddress() == "":
			return invalid("spec.harness.external needs one of address, command or python")
		}
	}
	b := spec.GetHarness().GetBuiltin()
	for _, s := range b.GetTools().GetMcp() {
		if (s.GetUrl() == "") == (len(s.GetCommand()) == 0) {
			return invalid("every MCP server needs exactly one of url or command")
		}
		if len(s.GetCommand()) > 0 {
			if err := m.trustCommand(s.GetCommand(), "MCP server "+s.GetName()); err != nil {
				return err
			}
		}
	}
	for _, f := range b.GetTools().GetFaults() {
		if !slices.Contains([]string{"timeout", "error", "malformed"}, f.GetKind()) || f.GetRate() < 0 || f.GetRate() > 1 {
			return invalid("tool faults need a kind (timeout, error or malformed) and a rate from 0 to 1")
		}
	}
	switch rec := b.GetRecording(); rec.GetMode() {
	case "", "off":
	case "record", "replay":
		if _, err := m.recordingDir(rec.GetDir(), rec.GetMode() == "record"); err != nil {
			return err
		}
	default:
		return invalid("spec.harness.builtin.recording.mode must be off, record or replay")
	}
	if p := env.GetChecker().GetParser(); strings.Contains(p, ":") && !strings.HasPrefix(p, "junit:") {
		if err := m.trustPython(p, "spec.environment.checker.parser"); err != nil {
			return err
		}
	}
	switch n := env.GetSandbox().GetNetwork(); n {
	case "", "deny", "allow":
	case "allowlist":
		if len(env.GetSandbox().GetAllowHosts()) == 0 {
			return invalid("spec.environment.sandbox: network allowlist needs allow_hosts")
		}
	default:
		return invalid("spec.environment.sandbox.network must be deny, allowlist or allow, not %q", n)
	}
	return nil
}

func (m *Manager) trustCommand(argv []string, what string) error {
	for _, c := range m.opts.Agents.TrustedCommands {
		if slices.Equal(c, argv) {
			return nil
		}
	}
	return invalid("%s: %q would run on the server's worker, outside the sandbox; list it under agents.trusted_commands in the server config to allow it", what, argv)
}

func (m *Manager) trustPython(ref, what string) error {
	if strings.HasPrefix(ref, "evalsi_") || slices.Contains(m.opts.Agents.TrustedPython, ref) {
		return nil
	}
	return invalid("%s: %q would run on the server's worker; list it under agents.trusted_python in the server config to allow it", what, ref)
}

// recordingDir resolves a cassette directory inside datasets_dir, creating
// it for recording.
func (m *Manager) recordingDir(rel string, create bool) (string, error) {
	if rel == "" {
		rel = "cassettes"
	}
	if m.opts.DatasetsDir == "" {
		return "", invalid("recording needs datasets_dir in the server config")
	}
	if !datasets.Local(m.opts.DatasetsDir) {
		return "", invalid("recording needs a local datasets_dir; this server keeps datasets in object storage")
	}
	clean := filepath.Clean(rel)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", invalid("recording.dir must be relative to the server's datasets_dir")
	}
	if create {
		root, err := filepath.EvalSymlinks(m.opts.DatasetsDir)
		if err != nil {
			return "", err
		}
		if err := os.MkdirAll(filepath.Join(root, clean), 0o755); err != nil {
			return "", err
		}
	}
	return m.resolvePath(clean, "")
}

// workerSpec is the spec as the worker gets it: the recording directory
// resolved inside datasets_dir.
func (m *Manager) workerSpec(spec *evalsiv1alpha1.RunSpec) (*evalsiv1alpha1.RunSpec, error) {
	rec := spec.GetHarness().GetBuiltin().GetRecording()
	if rec.GetMode() == "" || rec.GetMode() == "off" {
		return spec, nil
	}
	dir, err := m.recordingDir(rec.GetDir(), rec.GetMode() == "record")
	if err != nil {
		return nil, err
	}
	out := proto.Clone(spec).(*evalsiv1alpha1.RunSpec)
	out.GetHarness().GetBuiltin().GetRecording().Dir = dir
	return out, nil
}
