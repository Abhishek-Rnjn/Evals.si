package credentials

import (
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/structpb"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

func TestValidate(t *testing.T) {
	for cfg, want := range map[*Config]string{
		{Grants: []Grant{{Env: "OPENAI_API_KEY", Projects: []string{"*"}, Hosts: []string{"api.openai.com", "*.openai.azure.com"}}}}: "",
		{Grants: []Grant{{Env: "OPENAI-KEY", Projects: []string{"*"}}}}:                                                              "must be a variable name",
		{Grants: []Grant{{Env: "K"}}}:                                                           "projects is required",
		{Grants: []Grant{{Env: "K", Projects: []string{""}}}}:                                   "empty name",
		{Grants: []Grant{{Env: "K", Projects: []string{"a"}, Hosts: []string{"https://x.io"}}}}: "not a lowercase host",
		{Grants: []Grant{{Env: "K", Projects: []string{"a"}, Hosts: []string{"*"}}}}:            "not a lowercase host",
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
		cfg  Config
		auth bool
		want bool
	}{
		{Config{}, false, false},
		{Config{}, true, true},
		{Config{Grants: []Grant{{Env: "K", Projects: []string{"*"}}}}, false, true},
		{Config{Enforce: &off}, true, false},
	}
	for _, c := range cases {
		if got := NewPolicy(c.cfg, c.auth).Enforced(); got != c.want {
			t.Errorf("%+v auth=%v: enforced %v", c.cfg, c.auth, got)
		}
	}
	var nilPolicy *Policy
	if err := nilPolicy.Check("p", []Use{{Env: "ANY", Field: "f"}}); err != nil {
		t.Errorf("a nil policy checks nothing: %v", err)
	}
}

func TestCheck(t *testing.T) {
	p := NewPolicy(Config{Grants: []Grant{
		{Env: "OPENAI_API_KEY", Projects: []string{"*"}, Hosts: []string{"api.openai.com"}},
		{Env: "AZURE_KEY", Projects: []string{"support"}, Hosts: []string{"*.openai.azure.com"}},
		{Env: "AGENT_TOKEN", Projects: []string{"support"}},
		// A second grant widens the first for one project.
		{Env: "OPENAI_API_KEY", Projects: []string{"research"}, Hosts: []string{"vllm.lab.internal"}},
	}}, true)
	for _, c := range []struct {
		project string
		use     Use
		want    string
	}{
		{"support", Use{Env: "OPENAI_API_KEY", URL: "https://api.openai.com/v1"}, ""},
		{"support", Use{Env: "OPENAI_API_KEY", URL: "https://API.OpenAI.com.:443/v1"}, ""},
		{"support", Use{Env: "OPENAI_API_KEY", URL: "https://api.openai.com.attacker.example/v1"}, "api.openai.com.attacker.example"},
		{"support", Use{Env: "OPENAI_API_KEY", URL: "https://user@evil.example/@api.openai.com"}, "evil.example"},
		{"support", Use{Env: "OPENAI_API_KEY", URL: "https://vllm.lab.internal/v1"}, "vllm.lab.internal"},
		{"research", Use{Env: "OPENAI_API_KEY", URL: "https://vllm.lab.internal/v1"}, ""},
		{"support", Use{Env: "OPENAI_API_KEY"}, "without a URL"},
		{"support", Use{Env: "OPENAI_API_KEY", URL: "not a url"}, "without a URL"},
		{"support", Use{Env: "AZURE_KEY", URL: "https://eastus.openai.azure.com/"}, ""},
		{"support", Use{Env: "AZURE_KEY", URL: "https://openai.azure.com/"}, "openai.azure.com"},
		{"checkout", Use{Env: "AZURE_KEY", URL: "https://eastus.openai.azure.com/"}, `project "checkout"`},
		{"support", Use{Env: "AGENT_TOKEN", URL: "https://anywhere.example"}, ""},
		{"support", Use{Env: "AGENT_TOKEN", Sandbox: true}, ""},
		{"support", Use{Env: "AZURE_KEY", Sandbox: true}, "sandbox"},
		{"support", Use{Env: "HOME", URL: "https://api.openai.com"}, "HOME"},
	} {
		err := p.Check(c.project, []Use{c.use})
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

func TestSpecUses(t *testing.T) {
	params, _ := structpb.NewStruct(map[string]any{
		"url": "https://rm.example", "api_key_env": "RM_KEY", "headers_env": map[string]any{"X-Org": "ORG_ID"}, "token_env": "none",
	})
	harnessCfg, _ := structpb.NewStruct(map[string]any{"base_url": "https://h.example", "secret_env": "H_SECRET"})
	spec := &evalsiv1alpha1.RunSpec{
		Target: &evalsiv1alpha1.Target{Connector: "anthropic", Model: "m"},
		Harness: &evalsiv1alpha1.Harness{Kind: &evalsiv1alpha1.Harness_Builtin{Builtin: &evalsiv1alpha1.BuiltinHarness{
			Tools: &evalsiv1alpha1.Tools{Mcp: []*evalsiv1alpha1.MCPServer{{Name: "crm", Url: "https://crm.example/mcp", HeadersEnv: map[string]string{"Authorization": "CRM_TOKEN"}}}},
		}}},
		Evaluators: []*evalsiv1alpha1.EvaluatorRef{{Ref: "reward-model", Params: params}},
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
		"RM_KEY":            "https://rm.example",
		"ORG_ID":            "https://rm.example",
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
