package runs

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
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
		if f := agent.GetCli().GetOutputFormat(); !cliOutputFormats[f] {
			return invalid("spec.target.agent.cli.output_format %q is not one of dsh-json, claude-stream-json, codex-json, gemini-json, pi-json", f)
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
		if err := validateMCPOptions(s, b.GetUserSimulator() != nil); err != nil {
			return err
		}
		if len(s.GetCommand()) > 0 {
			if err := m.trustCommand(s.GetCommand(), "MCP server "+s.GetName()); err != nil {
				return err
			}
		}
	}
	if p := b.GetProfile(); p != "" {
		name, ver, _ := strings.Cut(p, "@")
		if !slices.Contains(harnessProfiles, name) {
			return invalid("spec.harness.builtin.profile: unknown profile %q (choose from %s)", name, strings.Join(harnessProfiles, ", "))
		}
		if v, err := strconv.Atoi(ver); ver != "" && (err != nil || v < 1) {
			return invalid("spec.harness.builtin.profile: %q is not name or name@version", p)
		}
		if len(b.GetTools().GetBuiltin()) > 0 {
			return invalid("spec.harness.builtin.profile sets the toolset; remove tools.builtin or the profile")
		}
	}
	if len(b.GetInterjections()) > maxInterjections {
		return invalid("spec.harness.builtin.interjections can hold at most %d messages", maxInterjections)
	}
	for i, j := range b.GetInterjections() {
		if j.GetAtStep() < 1 || strings.TrimSpace(j.GetMessage()) == "" || len(j.GetMessage()) > 4000 {
			return invalid("spec.harness.builtin.interjections[%d] needs at_step of at least 1 and a message of 1 to 4000 characters", i)
		}
	}
	for _, name := range b.GetTools().GetBuiltin() {
		if !slices.Contains(builtinTools, name) {
			return invalid("spec.harness.builtin.tools.builtin: unknown tool %q (choose from %s)", name, strings.Join(builtinTools, ", "))
		}
	}
	noSandbox := b.GetTools() != nil && b.GetTools().Sandbox != nil && !b.GetTools().GetSandbox()
	// fresh-rounds keeps its notes in the memory tool, which it adds on its own.
	if noSandbox && b.GetContext().GetStrategy() == "fresh-rounds" {
		return invalid("spec.harness.builtin.context: fresh-rounds keeps its notes with the memory tool, which needs the sandbox tools that tools.sandbox turned off")
	}
	if b.GetProfile() != "" && noSandbox {
		return invalid("spec.harness.builtin.profile needs the sandbox tools, which tools.sandbox turned off")
	}
	if len(b.GetTools().GetBuiltin()) > 0 && noSandbox {
		return invalid("spec.harness.builtin.tools.builtin needs the sandbox tools, which tools.sandbox turned off")
	}
	for _, f := range b.GetTools().GetFaults() {
		if !slices.Contains([]string{"timeout", "error", "malformed"}, f.GetKind()) || f.GetRate() < 0 || f.GetRate() > 1 {
			return invalid("tool faults need a kind (timeout, error or malformed) and a rate from 0 to 1")
		}
	}
	if b.GetDelegateSteps() < 0 || b.GetDelegateSteps() > maxDelegateSteps {
		return invalid("spec.harness.builtin.delegate_steps must be from 0 to %d", maxDelegateSteps)
	}
	if err := validateContext(b.GetContext()); err != nil {
		return err
	}
	if len(b.GetHooks()) > maxHooks {
		return invalid("spec.harness.builtin.hooks can hold at most %d hooks", maxHooks)
	}
	for i, h := range b.GetHooks() {
		mod, class, ok := strings.Cut(h.GetPython(), ":")
		if !ok || mod == "" || class == "" {
			return invalid("spec.harness.builtin.hooks[%d].python must be module:Class", i)
		}
		if err := m.trustPython(h.GetPython(), fmt.Sprintf("spec.harness.builtin.hooks[%d].python", i)); err != nil {
			return err
		}
	}
	switch rec := b.GetRecording(); rec.GetMode() {
	case "", "off":
	case "record", "replay", "resume", "fork":
		if _, err := m.recordingDir(rec.GetDir(), rec.GetMode() != "replay"); err != nil {
			return err
		}
		if rec.GetBranchAtStep() < 0 {
			return invalid("spec.harness.builtin.recording.branch_at_step cannot be negative")
		}
		if rec.GetMode() == "fork" && rec.GetBranchAtStep() == 0 {
			return invalid("spec.harness.builtin.recording: mode fork needs branch_at_step above 0")
		}
		if rec.GetForkAs() != "" && (rec.GetMode() != "fork" || !forkName.MatchString(rec.GetForkAs())) {
			return invalid("spec.harness.builtin.recording.fork_as is for mode fork and takes letters, digits, dots, dashes and underscores")
		}
	default:
		return invalid("spec.harness.builtin.recording.mode must be off, record, replay, resume or fork")
	}
	if err := validateProtect(env.GetChecker()); err != nil {
		return err
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
	dir, err := m.recordingDir(rec.GetDir(), rec.GetMode() != "replay")
	if err != nil {
		return nil, err
	}
	out := proto.Clone(spec).(*evalsiv1alpha1.RunSpec)
	out.GetHarness().GetBuiltin().GetRecording().Dir = dir
	return out, nil
}

// forkName is what a forked log may be called: a file name, never a path.
var forkName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// maxDelegateSteps bounds a sub-agent's model calls.
const maxDelegateSteps = 200

// maxRounds bounds the rounds of a fresh-rounds run.
const maxRounds = 50

// maxHooks bounds the hooks of one run.
const maxHooks = 8

// harnessProfiles are the built-in agent's profiles (python/evalsi-harness profiles.py).
var harnessProfiles = []string{"minimal", "standard", "code-mode"}

// builtinTools are the optional built-in tools a spec may list.
var builtinTools = []string{"edit_file", "glob", "grep", "apply_patch", "memory", "session_search", "delegate", "tool_search", "code_mode"}

// minContextWindow is the smallest window a strategy can work in: below it the
// summary and the kept turns alone would not fit.
const minContextWindow = 4000

// validateContext checks the context management a spec asks for, so a bad
// value is refused here rather than failing every trial.
func validateContext(c *evalsiv1alpha1.ContextManagement) error {
	if c == nil {
		return nil
	}
	const f = "spec.harness.builtin.context"
	switch c.GetStrategy() {
	case "", "none":
	case "fresh-rounds":
		if c.GetRounds() < 0 || c.GetRounds() > maxRounds || c.GetRoundSteps() < 0 {
			return invalid("%s: rounds must be from 0 to %d and round_steps cannot be negative", f, maxRounds)
		}
	case "clear-tool-results", "compact", "evidence-state":
		if c.GetWindowTokens() < minContextWindow {
			return invalid("%s.window_tokens must be at least %d for strategy %s", f, minContextWindow, c.GetStrategy())
		}
	default:
		return invalid("%s.strategy must be none, clear-tool-results, compact, fresh-rounds or evidence-state, not %q", f, c.GetStrategy())
	}
	trigger, target := c.GetTrigger(), c.GetTarget()
	if trigger == 0 {
		trigger = 0.8
	}
	if target == 0 {
		target = 0.5
	}
	if trigger <= 0 || trigger > 1 || target <= 0 || target >= trigger {
		return invalid("%s: need 0 < target < trigger <= 1 (defaults 0.5 and 0.8)", f)
	}
	if c.GetKeepRecent() < 0 || c.GetKeepTurns() < 0 || c.GetSummaryMaxTokens() < 0 || c.GetWindowTokens() < 0 {
		return invalid("%s: keep_recent, keep_turns, summary_max_tokens and window_tokens cannot be negative", f)
	}
	return nil
}

// maxMCPTimeout bounds a request to an MCP server, in seconds.
const maxMCPTimeout = 3600

// validateMCPOptions checks an MCP server's timeouts, OAuth and elicitation.
func validateMCPOptions(s *evalsiv1alpha1.MCPServer, hasUser bool) error {
	f := "spec.harness.builtin.tools.mcp " + s.GetName()
	if s.GetTimeoutS() < 0 || s.GetTimeoutS() > maxMCPTimeout {
		return invalid("%s: timeout_s must be from 0 to %d", f, maxMCPTimeout)
	}
	for name, t := range s.GetToolTimeoutS() {
		if name == "" || t < 1 || t > maxMCPTimeout {
			return invalid("%s: tool_timeout_s needs a tool name and from 1 to %d seconds", f, maxMCPTimeout)
		}
	}
	if s.GetElicitation() && !hasUser {
		return invalid("%s: elicitation needs spec.harness.builtin.user_simulator to answer", f)
	}
	if o := s.GetOauth(); o != nil {
		u, err := url.Parse(o.GetTokenUrl())
		if len(s.GetCommand()) > 0 {
			return invalid("%s: oauth is for servers reached by url", f)
		}
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return invalid("%s: oauth.token_url must be an http or https URL", f)
		}
		if o.GetClientIdEnv() == "" || o.GetClientSecretEnv() == "" {
			return invalid("%s: oauth needs client_id_env and client_secret_env", f)
		}
	}
	return nil
}

// cliOutputFormats are the structured output formats the harness reads for a
// CLI agent (python/evalsi-harness formats.py); empty is plain output.
var cliOutputFormats = map[string]bool{
	"":                   true,
	"dsh-json":           true,
	"claude-stream-json": true,
	"codex-json":         true,
	"gemini-json":        true,
	"pi-json":            true,
}

// maxInterjections bounds the scripted mid-run user messages.
const maxInterjections = 20

// maxProtect bounds the patterns a checker may protect.
const maxProtect = 50

func validateProtect(c *evalsiv1alpha1.Checker) error {
	const f = "spec.environment.checker"
	if m := c.GetOnChange(); m != "" && m != "fail" && m != "restore" {
		return invalid("%s.on_change must be fail or restore", f)
	}
	if len(c.GetProtect()) > maxProtect {
		return invalid("%s.protect can hold at most %d patterns", f, maxProtect)
	}
	for _, p := range c.GetProtect() {
		if p == "" || len(p) > 120 || strings.HasPrefix(p, "/") || slices.Contains(strings.Split(p, "/"), "..") {
			return invalid("%s.protect: %q must be a relative pattern of at most 120 characters, without ..", f, p)
		}
	}
	return nil
}
