package server

import (
	"context"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/structpb"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1/evalsiv1alpha1connect"
	"github.com/abhishek-rnjn/evals.si/internal/auth"
	"github.com/abhishek-rnjn/evals.si/internal/authz"
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

// Refusals are audited as the caller who sent them, counted in /metrics,
// and plain HTTP to a host-limited grant's host is refused.
func TestCredentialRefusalsAreAudited(t *testing.T) {
	s := startAuthServer(t, func(c *config.Config) {
		c.Credentials.Grants = append(c.Credentials.Grants,
			credentials.Grant{Env: "MODELS_KEY", Projects: []string{"support"}, Hosts: []string{"models.example"}})
	})
	ctx := context.Background()
	runs := evalsiv1alpha1connect.NewRunServiceClient(s.http, s.url, as(s.keys["runner"]))
	spec := runSpec("qwen3")
	spec.Target.BaseUrl, spec.Target.ApiKeyEnv = "http://models.example/v1", "MODELS_KEY"
	_, err := runs.CreateRun(ctx, connect.NewRequest(&evalsiv1alpha1.CreateRunRequest{Project: "support", Spec: spec}))
	if codeOf(err) != connect.CodePermissionDenied || !strings.Contains(err.Error(), "over plain http") {
		t.Fatalf("plain http: %v", err)
	}
	spec.Target.BaseUrl = "https://models.example/v1"
	if _, err := runs.CreateRun(ctx, connect.NewRequest(&evalsiv1alpha1.CreateRunRequest{Project: "support", Spec: spec, ValidateOnly: true})); err != nil {
		t.Fatalf("https: %v", err)
	}

	owner := evalsiv1alpha1connect.NewAuthServiceClient(s.http, s.url, as(s.keys["owner"]))
	events, err := owner.ListAuditEvents(ctx, connect.NewRequest(&evalsiv1alpha1.ListAuditEventsRequest{Action: "credentials.use"}))
	if err != nil {
		t.Fatal(err)
	}
	var found *evalsiv1alpha1.AuditEvent
	for _, ev := range events.Msg.GetEvents() {
		if ev.GetResource() == "env:MODELS_KEY" {
			found = ev
		}
	}
	if found == nil || found.GetAllowed() || found.GetPrincipal() != "key:runner" || found.GetProject() != "support" ||
		!strings.HasSuffix(found.GetProcedure(), "/CreateRun") || !strings.Contains(found.GetReason(), "plain http") {
		t.Errorf("audit event %v (all: %v)", found, events.Msg.GetEvents())
	}

	req, _ := http.NewRequest(http.MethodGet, s.url+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+s.keys["owner"])
	resp, err := s.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), `evalsi_credential_denied_total{project="support",kind="env",name="MODELS_KEY"} 1`) {
		t.Errorf("metrics:\n%s", body)
	}
}

// A judge with projects is open to those projects only.
func TestJudgeProjects(t *testing.T) {
	s := startAuthServer(t, func(c *config.Config) {
		c.Judges = map[string]config.Judge{
			"claude": {Provider: "anthropic", Model: "m", Projects: []string{"support"}},
			"shared": {Provider: "anthropic", Model: "m"},
		}
		c.DefaultJudge = "claude"
	})
	ctx := context.Background()
	create := func(who, project, judge string) error {
		spec := runSpec("qwen3", "judge-score")
		spec.Judge = judge
		_, err := evalsiv1alpha1connect.NewRunServiceClient(s.http, s.url, as(s.keys[who])).CreateRun(ctx,
			connect.NewRequest(&evalsiv1alpha1.CreateRunRequest{Project: project, Spec: spec, ValidateOnly: true}))
		return err
	}
	if err := create("runner", "support", ""); err != nil {
		t.Errorf("support with its judge (the default): %v", err)
	}
	// checkout has no grant for the target's key either; the judge is checked first.
	if err := create("checkout-runner", "checkout", ""); codeOf(err) != connect.CodePermissionDenied || !strings.Contains(err.Error(), `judge "claude"`) {
		t.Errorf("checkout with support's judge: %v", err)
	}
	if err := create("checkout-runner", "checkout", "shared"); err != nil && strings.Contains(err.Error(), "judge") {
		t.Errorf("checkout with an open judge: %v", err)
	}
	eval := evalsiv1alpha1connect.NewEvaluationServiceClient(s.http, s.url, as(s.keys["checkout-runner"]))
	_, err := eval.Evaluate(ctx, connect.NewRequest(&evalsiv1alpha1.EvaluateRequest{
		Project: "checkout", Evaluators: []*evalsiv1alpha1.EvaluatorRef{{Ref: "judge-score"}}, Judge: "claude",
		Records: []*evalsiv1alpha1.Record{{Input: text("q"), Output: text("a")}},
	}))
	if codeOf(err) != connect.CodePermissionDenied {
		t.Errorf("Evaluate with support's judge: %v", err)
	}

	cat := func(who string) evalsiv1alpha1connect.CatalogServiceClient {
		return evalsiv1alpha1connect.NewCatalogServiceClient(s.http, s.url, as(s.keys[who]))
	}
	list, err := cat("checkout-runner").ListCredentials(ctx, connect.NewRequest(&evalsiv1alpha1.ListCredentialsRequest{Project: "checkout"}))
	if err != nil || !slices.Equal(list.Msg.GetJudges(), []string{"shared"}) || len(list.Msg.GetGrants()) != 0 || !list.Msg.GetEnforced() {
		t.Errorf("checkout's credentials: %v %v", list, err)
	}
	list, err = cat("viewer").ListCredentials(ctx, connect.NewRequest(&evalsiv1alpha1.ListCredentialsRequest{Project: "support"}))
	if err != nil || !slices.Equal(list.Msg.GetJudges(), []string{"claude", "shared"}) || len(list.Msg.GetGrants()) != 1 ||
		list.Msg.GetGrants()[0].GetEnv() != "OPENAI_API_KEY" || !slices.Equal(list.Msg.GetGrants()[0].GetHosts(), []string{"127.0.0.1"}) {
		t.Errorf("support's credentials: %v %v", list, err)
	}
	if _, err := cat("checkout-runner").ListCredentials(ctx, connect.NewRequest(&evalsiv1alpha1.ListCredentialsRequest{Project: "support"})); codeOf(err) != connect.CodePermissionDenied {
		t.Errorf("another project's credentials: %v", err)
	}
}

// validate_only checks a run or a policy without storing it.
func TestValidateOnly(t *testing.T) {
	s := startAuthServer(t, nil)
	ctx := context.Background()
	runs := evalsiv1alpha1connect.NewRunServiceClient(s.http, s.url, as(s.keys["runner"]))
	resp, err := runs.CreateRun(ctx, connect.NewRequest(&evalsiv1alpha1.CreateRunRequest{Project: "support", Spec: runSpec("qwen3"), ValidateOnly: true}))
	if err != nil || resp.Msg.GetRun() != nil {
		t.Fatalf("validate only: %v %v", resp, err)
	}
	listed, _ := runs.ListRuns(ctx, connect.NewRequest(&evalsiv1alpha1.ListRunsRequest{Project: "support"}))
	if n := len(listed.Msg.GetRuns()); n != 0 {
		t.Errorf("%d runs stored", n)
	}
	bad := runSpec("qwen3")
	bad.Target.ApiKeyEnv = "DATABASE_URL"
	if _, err := runs.CreateRun(ctx, connect.NewRequest(&evalsiv1alpha1.CreateRunRequest{Project: "support", Spec: bad, ValidateOnly: true})); codeOf(err) != connect.CodePermissionDenied {
		t.Errorf("validate only, ungranted: %v", err)
	}
	// Admission checks count like any other refusal (per replica: sum them).
	req, _ := http.NewRequest(http.MethodGet, s.url+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+s.keys["owner"])
	if resp, err := s.http.Do(req); err != nil {
		t.Fatal(err)
	} else {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if !strings.Contains(string(body), `evalsi_credential_denied_total{project="support",kind="env",name="DATABASE_URL"} 1`) {
			t.Errorf("validate-only refusal not counted:\n%s", body)
		}
	}
	mon := evalsiv1alpha1connect.NewMonitorServiceClient(s.http, s.url, as(s.keys["editor"]))
	policy := &evalsiv1alpha1.OnlineEvalPolicy{Name: "dry", Project: "support", Stages: []*evalsiv1alpha1.CascadeStage{{Evaluators: []*evalsiv1alpha1.EvaluatorRef{{Ref: "exact-match"}}}}}
	if _, err := mon.ApplyPolicy(ctx, connect.NewRequest(&evalsiv1alpha1.ApplyPolicyRequest{Policy: policy, ValidateOnly: true})); err != nil {
		t.Fatal(err)
	}
	if got, _ := mon.ListPolicies(ctx, connect.NewRequest(&evalsiv1alpha1.ListPoliciesRequest{Project: "support"})); len(got.Msg.GetPolicies()) != 0 {
		t.Errorf("validate-only policy stored: %v", got.Msg.GetPolicies())
	}
}

// Changed grants and judge projects apply without a restart.
func TestCredentialsReload(t *testing.T) {
	old := reloadInterval
	reloadInterval = 20 * time.Millisecond
	t.Cleanup(func() { reloadInterval = old })
	var next atomic.Pointer[config.Config]
	s := startAuthServer(t, func(c *config.Config) {
		c.Reload = func() (config.Config, error) {
			if n := next.Load(); n != nil {
				return *n, nil
			}
			return *c, nil
		}
		// The served copy's Reload must see later changes: keep a copy
		// to change.
		cp := *c
		next.Store(&cp)
	})
	ctx := context.Background()
	spec := runSpec("qwen3")
	spec.Target.ApiKeyEnv = "LATE_KEY"
	create := func() error {
		_, err := evalsiv1alpha1connect.NewRunServiceClient(s.http, s.url, as(s.keys["runner"])).CreateRun(ctx,
			connect.NewRequest(&evalsiv1alpha1.CreateRunRequest{Project: "support", Spec: spec, ValidateOnly: true}))
		return err
	}
	if err := create(); codeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("before the grant: %v", err)
	}
	cp := *next.Load()
	cp.Credentials.Grants = append(slices.Clone(cp.Credentials.Grants), credentials.Grant{Env: "LATE_KEY", Projects: []string{"support"}})
	next.Store(&cp)
	deadline := time.Now().Add(5 * time.Second)
	for create() != nil {
		if time.Now().After(deadline) {
			t.Fatal("the new grant was not picked up")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// A stored guardrail whose evaluator names the variable works while
	// the grant lasts, and stops when it is withdrawn.
	params, _ := structpb.NewStruct(map[string]any{"url": "https://scorer.example", "api_key_env": "LATE_KEY"})
	guard := func(who string) evalsiv1alpha1connect.GuardrailServiceClient {
		return evalsiv1alpha1connect.NewGuardrailServiceClient(s.http, s.url, as(s.keys[who]))
	}
	g := &evalsiv1alpha1.Guardrail{Name: "scored", Project: "support", Evaluators: []*evalsiv1alpha1.EvaluatorRef{{Ref: "reward-model", Params: params}}}
	if _, err := guard("editor").ApplyGuardrail(ctx, connect.NewRequest(&evalsiv1alpha1.ApplyGuardrailRequest{Guardrail: g})); err != nil {
		t.Fatal(err)
	}
	check := func() error {
		_, err := guard("owner").Check(ctx, connect.NewRequest(&evalsiv1alpha1.CheckRequest{Project: "support", Guardrail: "scored", Content: "hi"}))
		return err
	}
	if err := check(); err != nil {
		t.Fatalf("granted guardrail: %v", err)
	}
	withdrawn := *next.Load()
	withdrawn.Credentials.Grants = slices.DeleteFunc(slices.Clone(withdrawn.Credentials.Grants), func(g credentials.Grant) bool { return g.Env == "LATE_KEY" })
	next.Store(&withdrawn)
	deadline = time.Now().Add(5 * time.Second)
	for codeOf(check()) != connect.CodePermissionDenied {
		if time.Now().After(deadline) {
			t.Fatalf("the guardrail still runs after the grant was withdrawn: %v", check())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Refusals the enforcement point did not make itself (a privilege
// escalation found by AuthService) and rejected credentials are audited too.
func TestEscalationAndUnauthenticatedAreAudited(t *testing.T) {
	s := startAuthServer(t, nil)
	ctx := context.Background()
	keyAdmin := evalsiv1alpha1connect.NewAuthServiceClient(s.http, s.url, as(s.keys["key-admin"]))
	_, err := keyAdmin.CreateAPIKey(ctx, connect.NewRequest(&evalsiv1alpha1.CreateAPIKeyRequest{
		Name:  "sneaky",
		Roles: map[string]*evalsiv1alpha1.RoleList{"support": {Roles: []string{"runner"}}},
	}))
	if codeOf(err) != connect.CodePermissionDenied || !strings.Contains(err.Error(), "privilege escalation") {
		t.Fatalf("escalation: %v", err)
	}
	anon := evalsiv1alpha1connect.NewRunServiceClient(s.http, s.url, as("not-a-key"))
	if _, err := anon.ListRuns(ctx, connect.NewRequest(&evalsiv1alpha1.ListRunsRequest{Project: "support"})); codeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("bad key: %v", err)
	}

	owner := evalsiv1alpha1connect.NewAuthServiceClient(s.http, s.url, as(s.keys["owner"]))
	events, err := owner.ListAuditEvents(ctx, connect.NewRequest(&evalsiv1alpha1.ListAuditEventsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var escalation, unauth bool
	for _, ev := range events.Msg.GetEvents() {
		switch {
		case ev.GetResource() == "apikey/sneaky" && !ev.GetAllowed() && ev.GetPrincipal() == "key:key-admin" &&
			strings.Contains(ev.GetReason(), "privilege escalation"):
			escalation = true
		case ev.GetAction() == "auth.authenticate" && !ev.GetAllowed() && strings.HasSuffix(ev.GetProcedure(), "/ListRuns"):
			unauth = true
		}
	}
	if !escalation || !unauth {
		t.Errorf("escalation audited %v, unauthenticated audited %v; events: %v", escalation, unauth, events.Msg.GetEvents())
	}
}

// Access rules see the judge evaluators will actually use: omitting the
// judge gets the same decision as naming the default.
func TestRulesSeeTheDefaultJudge(t *testing.T) {
	plain, hash := auth.NewAPIKey()
	s := startAuthServer(t, func(c *config.Config) {
		c.Judges = map[string]config.Judge{"claude": {Provider: "anthropic", Model: "m"}, "shared": {Provider: "anthropic", Model: "m"}}
		c.DefaultJudge = "claude"
		c.RBAC.Roles = append(c.RBAC.Roles, authz.Role{Name: "cheap-judges", Permissions: []string{"evaluations.run"},
			Condition: `resource.judge != "claude"`})
		c.Auth.APIKeys.Keys = append(c.Auth.APIKeys.Keys, auth.ConfigKey{Name: "cheap", Key: "sha256:" + hash,
			Roles: map[string][]string{"support": {"cheap-judges"}}})
	})
	eval := evalsiv1alpha1connect.NewEvaluationServiceClient(s.http, s.url, as(plain))
	evaluate := func(ref, judge string) error {
		_, err := eval.Evaluate(context.Background(), connect.NewRequest(&evalsiv1alpha1.EvaluateRequest{
			Project: "support", Evaluators: []*evalsiv1alpha1.EvaluatorRef{{Ref: ref}}, Judge: judge,
			Records: []*evalsiv1alpha1.Record{{Input: text("q"), Output: text("a"), Reference: text("a")}},
		}))
		return err
	}
	for _, judge := range []string{"claude", ""} {
		if err := evaluate("judge-score", judge); codeOf(err) != connect.CodePermissionDenied {
			t.Errorf("judge %q: %v, want PermissionDenied", judge, err)
		}
	}
	if err := evaluate("judge-score", "shared"); codeOf(err) == connect.CodePermissionDenied {
		t.Errorf("another judge: %v", err)
	}
	// An evaluator that needs no judge uses none, so the rule does not apply.
	if err := evaluate("exact-match", ""); codeOf(err) == connect.CodePermissionDenied {
		t.Errorf("no judge needed: %v", err)
	}
}

// Lists show exactly what single reads allow: a run or policy that uses the
// default judge through an omitted judge is hidden from a caller whose rule
// excludes that judge.
func TestListsSeeTheDefaultJudge(t *testing.T) {
	plain, hash := auth.NewAPIKey()
	s := startAuthServer(t, func(c *config.Config) {
		c.Judges = map[string]config.Judge{"claude": {Provider: "anthropic", Model: "m"}, "shared": {Provider: "anthropic", Model: "m"}}
		c.DefaultJudge = "claude"
		c.RBAC.Roles = append(c.RBAC.Roles, authz.Role{Name: "cheap-reader", Permissions: []string{"runs.read", "policies.read"},
			Condition: `resource.judge != "claude"`})
		c.Auth.APIKeys.Keys = append(c.Auth.APIKeys.Keys, auth.ConfigKey{Name: "cheap", Key: "sha256:" + hash,
			Roles: map[string][]string{"support": {"cheap-reader"}}})
	})
	ctx := context.Background()
	owner := evalsiv1alpha1connect.NewRunServiceClient(s.http, s.url, as(s.keys["owner"]))
	created := map[string]string{}
	for _, judge := range []string{"", "shared"} {
		resp, err := owner.CreateRun(ctx, connect.NewRequest(&evalsiv1alpha1.CreateRunRequest{Project: "support", Spec: func() *evalsiv1alpha1.RunSpec {
			spec := runSpec("qwen3", "judge-score")
			spec.Judge = judge
			return spec
		}()}))
		if err != nil {
			t.Fatal(err)
		}
		created[judge] = resp.Msg.GetRun().GetId()
	}
	runs := evalsiv1alpha1connect.NewRunServiceClient(s.http, s.url, as(plain))
	if _, err := runs.GetRun(ctx, connect.NewRequest(&evalsiv1alpha1.GetRunRequest{Id: created[""]})); codeOf(err) != connect.CodePermissionDenied {
		t.Errorf("GetRun of a default-judge run: %v", err)
	}
	list, err := runs.ListRuns(ctx, connect.NewRequest(&evalsiv1alpha1.ListRunsRequest{Project: "support"}))
	if err != nil {
		t.Fatal(err)
	}
	var listed []string
	for _, r := range list.Msg.GetRuns() {
		listed = append(listed, r.GetId())
	}
	if !slices.Equal(listed, []string{created["shared"]}) {
		t.Errorf("ListRuns = %v, want only the run on another judge (%s)", listed, created["shared"])
	}

	ownerMon := evalsiv1alpha1connect.NewMonitorServiceClient(s.http, s.url, as(s.keys["owner"]))
	for name, judge := range map[string]string{"default-judge": "", "other-judge": "shared"} {
		p := &evalsiv1alpha1.OnlineEvalPolicy{Name: name, Project: "support", Judge: judge,
			Stages: []*evalsiv1alpha1.CascadeStage{{Evaluators: []*evalsiv1alpha1.EvaluatorRef{{Ref: "judge-score"}}}}}
		if _, err := ownerMon.ApplyPolicy(ctx, connect.NewRequest(&evalsiv1alpha1.ApplyPolicyRequest{Policy: p})); err != nil {
			t.Fatal(err)
		}
	}
	mon := evalsiv1alpha1connect.NewMonitorServiceClient(s.http, s.url, as(plain))
	if _, err := mon.GetPolicyStats(ctx, connect.NewRequest(&evalsiv1alpha1.GetPolicyStatsRequest{Name: "default-judge"})); codeOf(err) != connect.CodePermissionDenied {
		t.Errorf("GetPolicyStats of a default-judge policy: %v", err)
	}
	policies, err := mon.ListPolicies(ctx, connect.NewRequest(&evalsiv1alpha1.ListPoliciesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if ps := policies.Msg.GetPolicies(); len(ps) != 1 || ps[0].GetName() != "other-judge" {
		t.Errorf("ListPolicies = %v, want only other-judge", ps)
	}
}
