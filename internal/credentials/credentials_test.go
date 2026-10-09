package credentials

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/structpb"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

func grants(gs ...Grant) Settings {
	return Settings{Credentials: Config{Grants: gs}, AuthEnabled: true}
}

func TestValidate(t *testing.T) {
	for cfg, want := range map[*Config]string{
		{Grants: []Grant{{Env: "OPENAI_API_KEY", Projects: []string{"*"}, Hosts: []string{"api.openai.com", "*.openai.azure.com"}}}}: "",
		{Grants: []Grant{{Env: "OPENAI-KEY", Projects: []string{"*"}}}}:                                                              "must be a variable name",
		{Grants: []Grant{{Env: "K"}}}:                                                           "projects is required",
		{Grants: []Grant{{Env: "K", Projects: []string{""}}}}:                                   "empty name",
		{Grants: []Grant{{Env: "K", Projects: []string{"a"}, Hosts: []string{"https://x.io"}}}}: "not a lowercase host",
		{Grants: []Grant{{Env: "K", Projects: []string{"a"}, Hosts: []string{"*"}}}}:            "not a lowercase host",
		{Grants: []Grant{{Env: "K", Projects: []string{"a"}, AllowHTTP: true}}}:                 "allow_http needs hosts",
	} {
		err := cfg.Validate()
		if (want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), want)) {
			t.Errorf("%+v: got %v, want %q", cfg.Grants, err, want)
		}
	}
}

func TestEnforcedByDefaultWithAuth(t *testing.T) {
	off := false
	cases := []struct {
		s    Settings
		want bool
	}{
		{Settings{}, false},
		{Settings{AuthEnabled: true}, true},
		{Settings{Credentials: Config{Grants: []Grant{{Env: "K", Projects: []string{"*"}}}}}, true},
		{Settings{Credentials: Config{Enforce: &off}, AuthEnabled: true}, false},
	}
	for _, c := range cases {
		if got := NewPolicy(c.s).Enforced(); got != c.want {
			t.Errorf("%+v: enforced %v", c.s, got)
		}
	}
	var nilPolicy *Policy
	if err := nilPolicy.Check(context.Background(), "p", []Use{{Env: "ANY", Field: "f"}}); err != nil {
		t.Errorf("a nil policy checks nothing: %v", err)
	}
}

func TestCheck(t *testing.T) {
	p := NewPolicy(grants(
		Grant{Env: "OPENAI_API_KEY", Projects: []string{"*"}, Hosts: []string{"api.openai.com"}},
		Grant{Env: "AZURE_KEY", Projects: []string{"support"}, Hosts: []string{"*.openai.azure.com"}},
		Grant{Env: "AGENT_TOKEN", Projects: []string{"support"}},
		// A second grant widens the first for one project.
		Grant{Env: "OPENAI_API_KEY", Projects: []string{"research"}, Hosts: []string{"vllm.lab.internal"}},
		Grant{Env: "LAB_KEY", Projects: []string{"research"}, Hosts: []string{"gpu.lab.internal"}, AllowHTTP: true},
	))
	for _, c := range []struct {
		project string
		use     Use
		want    string
	}{
		{"support", Use{Env: "OPENAI_API_KEY", URL: "https://api.openai.com/v1"}, ""},
		{"support", Use{Env: "OPENAI_API_KEY", URL: "https://API.OpenAI.com.:443/v1"}, ""},
		{"support", Use{Env: "OPENAI_API_KEY", URL: "https://api.openai.com.attacker.example/v1"}, "api.openai.com.attacker.example"},
		{"support", Use{Env: "OPENAI_API_KEY", URL: "https://user@evil.example/@api.openai.com"}, "evil.example"},
		// Plain HTTP to a granted host would put the key on the wire.
		{"support", Use{Env: "OPENAI_API_KEY", URL: "http://api.openai.com/v1"}, "over plain http"},
		{"research", Use{Env: "LAB_KEY", URL: "http://gpu.lab.internal:8000/v1"}, ""},
		{"support", Use{Env: "OPENAI_API_KEY", URL: "https://vllm.lab.internal/v1"}, "vllm.lab.internal"},
		{"research", Use{Env: "OPENAI_API_KEY", URL: "https://vllm.lab.internal/v1"}, ""},
		{"support", Use{Env: "OPENAI_API_KEY"}, "without a URL"},
		{"support", Use{Env: "OPENAI_API_KEY", URL: "not a url"}, "without a URL"},
		{"support", Use{Env: "AZURE_KEY", URL: "https://eastus.openai.azure.com/"}, ""},
		{"support", Use{Env: "AZURE_KEY", URL: "https://openai.azure.com/"}, "openai.azure.com"},
		{"checkout", Use{Env: "AZURE_KEY", URL: "https://eastus.openai.azure.com/"}, `project "checkout"`},
		{"support", Use{Env: "AGENT_TOKEN", URL: "http://anywhere.example"}, ""},
		{"support", Use{Env: "AGENT_TOKEN", Sandbox: true}, ""},
		{"support", Use{Env: "AZURE_KEY", Sandbox: true}, "sandbox"},
		{"support", Use{Env: "HOME", URL: "https://api.openai.com"}, "HOME"},
	} {
		err := p.Check(context.Background(), c.project, []Use{c.use})
		if c.want == "" {
			if err != nil {
				t.Errorf("%s %+v: %v", c.project, c.use, err)
			}
			continue
		}
		if connect.CodeOf(err) != connect.CodePermissionDenied || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s %+v: got %v, want denied mentioning %q", c.project, c.use, err, c.want)
		}
	}
}

func TestLoopbackIsAllowedOverHTTP(t *testing.T) {
	p := NewPolicy(grants(Grant{Env: "K", Projects: []string{"*"}, Hosts: []string{"localhost", "127.0.0.1"}}))
	for _, u := range []string{"http://localhost:8000/v1", "http://127.0.0.1:8000/v1"} {
		if err := p.Check(context.Background(), "p", []Use{{Env: "K", URL: u}}); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
}

func TestJudges(t *testing.T) {
	s := grants()
	s.Judges = map[string][]string{"claude": {"support"}, "open": nil, "everyone": {"*"}}
	p := NewPolicy(s)
	ctx := context.Background()
	for _, c := range []struct {
		project, judge string
		ok             bool
	}{
		{"support", "claude", true}, {"checkout", "claude", false}, {"checkout", "open", true},
		{"checkout", "everyone", true}, {"checkout", "unlisted", true}, {"checkout", "", true},
	} {
		err := p.CheckJudge(ctx, c.project, c.judge, "evaluators[0]")
		if c.ok != (err == nil) || (err != nil && connect.CodeOf(err) != connect.CodePermissionDenied) {
			t.Errorf("%s using %q: %v", c.project, c.judge, err)
		}
	}
	// Judge scopes hold even with grants off: they are listed on purpose.
	off := false
	s.Credentials.Enforce = &off
	if err := NewPolicy(s).CheckJudge(ctx, "checkout", "claude", "f"); err == nil {
		t.Error("judge scopes ignored with enforcement off")
	}
}

func TestDenialsAreReportedAndCounted(t *testing.T) {
	p := NewPolicy(grants(Grant{Env: "K", Projects: []string{"a"}}))
	var got []Denial
	p.OnDenied(func(_ context.Context, d Denial) { got = append(got, d) })
	ctx := context.Background()
	_ = p.Check(ctx, "b", []Use{{Env: "K", Field: "spec.target.api_key_env"}})
	_ = p.Check(ctx, "b", []Use{{Env: "K", Field: "spec.target.api_key_env"}})
	_ = p.Check(ctx, "a", []Use{{Env: "K", Field: "spec.target.api_key_env"}})
	if len(got) != 2 || got[0].Project != "b" || got[0].Subject != "env:K" || !strings.Contains(got[0].Reason, "may not use") {
		t.Errorf("denials %+v", got)
	}
	var buf bytes.Buffer
	p.WriteMetrics(&buf)
	if !strings.Contains(buf.String(), `evalsi_credential_denied_total{project="b",kind="env",name="K"} 2`) {
		t.Errorf("metrics:\n%s", buf.String())
	}
}

func TestUpdate(t *testing.T) {
	p := NewPolicy(grants())
	v := p.Version()
	ctx := context.Background()
	if err := p.Check(ctx, "a", []Use{{Env: "K"}}); err == nil {
		t.Fatal("ungranted before the update")
	}
	p.Update(grants(Grant{Env: "K", Projects: []string{"a"}}))
	if p.Version() == v {
		t.Error("version unchanged")
	}
	if err := p.Check(ctx, "a", []Use{{Env: "K"}}); err != nil {
		t.Errorf("granted after the update: %v", err)
	}
	enforced, gs := p.Grants("a")
	if !enforced || len(gs) != 1 || gs[0].Env != "K" {
		t.Errorf("Grants(a) = %v %+v", enforced, gs)
	}
	if _, gs := p.Grants("b"); len(gs) != 0 {
		t.Errorf("Grants(b) = %+v", gs)
	}
}

func TestSpecUses(t *testing.T) {
	harnessCfg, _ := structpb.NewStruct(map[string]any{"base_url": "https://h.example", "secret_env": "H_SECRET"})
	spec := &evalsiv1alpha1.RunSpec{
		Target: &evalsiv1alpha1.Target{Connector: "anthropic", Model: "m"},
		Harness: &evalsiv1alpha1.Harness{Kind: &evalsiv1alpha1.Harness_Builtin{Builtin: &evalsiv1alpha1.BuiltinHarness{
			Tools: &evalsiv1alpha1.Tools{Mcp: []*evalsiv1alpha1.MCPServer{{Name: "crm", Url: "https://crm.example/mcp", HeadersEnv: map[string]string{"Authorization": "CRM_TOKEN"}}}},
		}}},
	}
	uses, err := SpecUses(spec)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, u := range uses {
		got[u.Env] = u.URL
	}
	want := map[string]string{
		"ANTHROPIC_API_KEY": "https://api.anthropic.com", // the SDK's default variable and host
		"CRM_TOKEN":         "https://crm.example/mcp",
	}
	if len(got) != len(want) {
		t.Errorf("uses = %v", got)
	}
	for env, url := range want {
		if got[env] != url {
			t.Errorf("%s: %q, want %q (all: %v)", env, got[env], url, got)
		}
	}

	ext := &evalsiv1alpha1.RunSpec{
		Target: &evalsiv1alpha1.Target{Connector: "openai-compatible", Model: "m", BaseUrl: "http://localhost:8000/v1", ApiKeyEnv: None},
		Harness: &evalsiv1alpha1.Harness{Kind: &evalsiv1alpha1.Harness_External{External: &evalsiv1alpha1.ExternalHarness{
			Kind: &evalsiv1alpha1.ExternalHarness_Address{Address: "h:1"}, Config: harnessCfg,
		}}},
	}
	uses, err = SpecUses(ext)
	if err != nil || len(uses) != 1 || uses[0].Env != "H_SECRET" || uses[0].URL != "https://h.example" {
		t.Errorf("external harness config: %+v %v", uses, err)
	}

	defaultKey, _ := SpecUses(&evalsiv1alpha1.RunSpec{Target: &evalsiv1alpha1.Target{Connector: "openai-compatible", Model: "m", BaseUrl: "https://x.example/v1"}})
	if len(defaultKey) != 1 || defaultKey[0].Env != "OPENAI_API_KEY" {
		t.Errorf("openai-compatible without api_key_env reads OPENAI_API_KEY: %+v", defaultKey)
	}
	if _, err := SpecUses(&evalsiv1alpha1.RunSpec{Target: &evalsiv1alpha1.Target{Connector: "anthropic", Model: "m", ApiKeyEnv: None}}); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("anthropic with no key: %v", err)
	}
}

func TestParamUses(t *testing.T) {
	params, _ := structpb.NewStruct(map[string]any{
		"url": "https://rm.example", "endpoint": "https://other.example",
		"api_key_env": "RM_KEY", "headers_env": map[string]any{"X-Org": "ORG_ID"}, "token_env": "none",
		"key_var": "DECLARED", "label": "TEXT",
	})
	// key_var is declared as naming a variable sent to endpoint; _env
	// params count whether declared or not.
	schema, _ := structpb.NewStruct(map[string]any{"properties": map[string]any{
		"key_var": map[string]any{SecretKeyword: map[string]any{"sent_to": "endpoint"}},
		"label":   map[string]any{},
	}})
	got := map[string]string{}
	for _, u := range ParamUses("evaluators[0]", params, schema) {
		got[u.Env] = u.URL
	}
	want := map[string]string{"RM_KEY": "https://rm.example", "ORG_ID": "https://rm.example", "DECLARED": "https://other.example"}
	if len(got) != len(want) {
		t.Errorf("uses = %v", got)
	}
	for env, url := range want {
		if got[env] != url {
			t.Errorf("%s: %q, want %q", env, got[env], url)
		}
	}
	if d := Declared(schema); len(d) != 1 || d["key_var"] != "endpoint" {
		t.Errorf("Declared = %v", d)
	}
}
