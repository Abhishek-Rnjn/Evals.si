package bootstrap

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1/evalsiv1alpha1connect"
	"github.com/abhishek-rnjn/evals.si/operator/controllers"
)

// server is an in-memory evalsid: just enough of the auth, monitor and
// webhook services for the bootstrap Job.
type server struct {
	evalsiv1alpha1connect.AuthServiceClient
	evalsiv1alpha1connect.MonitorServiceClient
	evalsiv1alpha1connect.WebhookServiceClient

	whoamiFails int
	whoamiErr   error
	projects    map[string]bool
	keys        map[string]*evalsiv1alpha1.APIKey
	policies    map[string]*evalsiv1alpha1.OnlineEvalPolicy
	webhooks    map[string]*evalsiv1alpha1.Webhook
	calls       map[string]int
}

func newServer() *server {
	return &server{
		projects: map[string]bool{}, keys: map[string]*evalsiv1alpha1.APIKey{},
		policies: map[string]*evalsiv1alpha1.OnlineEvalPolicy{}, webhooks: map[string]*evalsiv1alpha1.Webhook{},
		calls: map[string]int{},
	}
}

func (s *server) WhoAmI(context.Context, *connect.Request[evalsiv1alpha1.WhoAmIRequest]) (*connect.Response[evalsiv1alpha1.WhoAmIResponse], error) {
	if s.whoamiErr != nil {
		return nil, s.whoamiErr
	}
	if s.whoamiFails > 0 {
		s.whoamiFails--
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("not ready"))
	}
	return connect.NewResponse(&evalsiv1alpha1.WhoAmIResponse{}), nil
}

func (s *server) CreateProject(_ context.Context, r *connect.Request[evalsiv1alpha1.CreateProjectRequest]) (*connect.Response[evalsiv1alpha1.CreateProjectResponse], error) {
	s.calls["CreateProject"]++
	if s.projects[r.Msg.GetName()] {
		return nil, connect.NewError(connect.CodeAlreadyExists, errors.New("exists"))
	}
	s.projects[r.Msg.GetName()] = true
	return connect.NewResponse(&evalsiv1alpha1.CreateProjectResponse{}), nil
}

func (s *server) ListAPIKeys(context.Context, *connect.Request[evalsiv1alpha1.ListAPIKeysRequest]) (*connect.Response[evalsiv1alpha1.ListAPIKeysResponse], error) {
	out := &evalsiv1alpha1.ListAPIKeysResponse{}
	for _, k := range s.keys {
		out.Keys = append(out.Keys, k)
	}
	return connect.NewResponse(out), nil
}

func (s *server) CreateAPIKey(_ context.Context, r *connect.Request[evalsiv1alpha1.CreateAPIKeyRequest]) (*connect.Response[evalsiv1alpha1.CreateAPIKeyResponse], error) {
	s.calls["CreateAPIKey"]++
	name := r.Msg.GetName()
	if s.keys[name] != nil {
		return nil, connect.NewError(connect.CodeAlreadyExists, errors.New("exists"))
	}
	s.keys[name] = &evalsiv1alpha1.APIKey{Name: name, Roles: r.Msg.GetRoles()}
	return connect.NewResponse(&evalsiv1alpha1.CreateAPIKeyResponse{Key: s.keys[name], Secret: "evk_secret_of_" + name}), nil
}

func (s *server) RevokeAPIKey(_ context.Context, r *connect.Request[evalsiv1alpha1.RevokeAPIKeyRequest]) (*connect.Response[evalsiv1alpha1.RevokeAPIKeyResponse], error) {
	s.calls["RevokeAPIKey"]++
	s.keys[r.Msg.GetName()].Revoked = true
	return connect.NewResponse(&evalsiv1alpha1.RevokeAPIKeyResponse{}), nil
}

func (s *server) ApplyPolicy(_ context.Context, r *connect.Request[evalsiv1alpha1.ApplyPolicyRequest]) (*connect.Response[evalsiv1alpha1.ApplyPolicyResponse], error) {
	s.calls["ApplyPolicy"]++
	s.policies[r.Msg.GetPolicy().GetName()] = r.Msg.GetPolicy()
	return connect.NewResponse(&evalsiv1alpha1.ApplyPolicyResponse{}), nil
}

func (s *server) ApplyWebhook(_ context.Context, r *connect.Request[evalsiv1alpha1.ApplyWebhookRequest]) (*connect.Response[evalsiv1alpha1.ApplyWebhookResponse], error) {
	s.calls["ApplyWebhook"]++
	s.webhooks[r.Msg.GetWebhook().GetName()] = r.Msg.GetWebhook()
	return connect.NewResponse(&evalsiv1alpha1.ApplyWebhookResponse{}), nil
}

func runner(srv *server, secrets Secrets) *Runner {
	return &Runner{
		API:     &controllers.API{Auth: srv, Monitor: srv, Webhooks: srv},
		Secrets: secrets,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Wait:    time.Second, Poll: time.Millisecond,
	}
}

const sample = `{
  "projects": [{"name": "studio", "description": "agent-studio"}],
  "api_keys": [{"name": "studio-ingest", "roles": {"studio": ["ingest"]}, "secret": {"name": "studio-evalsi-key"}}],
  "policies": [{"name": "studio-quality", "project": "studio", "sampling": {"rate": 1}}],
  "webhooks": [{"name": "studio-ci", "project": "studio", "url": "https://studio.example.com/hooks", "events": ["run.finished"], "secret": {"name": "studio-webhook"}}]
}`

func TestBootstrapCreatesAndIsIdempotent(t *testing.T) {
	cfg, err := Load([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	srv := newServer()
	secrets := KubeSecrets{Client: fake.NewSimpleClientset(), Namespace: "evalsi"}
	r := runner(srv, secrets)
	ctx := context.Background()
	if err := r.Run(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if !srv.projects["studio"] || srv.keys["studio-ingest"] == nil || srv.policies["studio-quality"] == nil || srv.webhooks["studio-ci"] == nil {
		t.Fatalf("not everything was made: %+v", srv)
	}
	data, ann, found, _ := secrets.Get(ctx, "studio-evalsi-key")
	if !found || string(data["api_key"]) != "evk_secret_of_studio-ingest" || ann[KeyNameAnnotation] != "studio-ingest" {
		t.Errorf("key Secret: %v %v %v", found, data, ann)
	}
	hook, _, found, _ := secrets.Get(ctx, "studio-webhook")
	if !found || !strings.HasPrefix(string(hook["secret"]), "whsec_") || srv.webhooks["studio-ci"].GetSecret() != string(hook["secret"]) {
		t.Errorf("webhook Secret: %v %v", found, hook)
	}
	if srv.webhooks["studio-ci"].GetEvents()[0] != evalsiv1alpha1.WebhookEvent_WEBHOOK_EVENT_RUN_FINISHED {
		t.Errorf("events: %v", srv.webhooks["studio-ci"].GetEvents())
	}

	// Again, as an upgrade does: nothing is created or rotated.
	before := srv.calls["CreateAPIKey"]
	hookBefore := string(hook["secret"])
	if err := r.Run(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if srv.calls["CreateAPIKey"] != before || srv.calls["RevokeAPIKey"] != 0 {
		t.Errorf("the rerun made keys: %v", srv.calls)
	}
	again, _, _, _ := secrets.Get(ctx, "studio-webhook")
	if string(again["secret"]) != hookBefore || srv.webhooks["studio-ci"].GetSecret() != hookBefore {
		t.Error("the webhook secret changed on a rerun")
	}
}

func TestBootstrapReplacesAKeyWhoseSecretIsGone(t *testing.T) {
	cfg, _ := Load([]byte(sample))
	srv := newServer()
	client := fake.NewSimpleClientset()
	secrets := KubeSecrets{Client: client, Namespace: "evalsi"}
	r := runner(srv, secrets)
	ctx := context.Background()
	if err := r.Run(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if err := client.CoreV1().Secrets("evalsi").Delete(ctx, "studio-evalsi-key", metaDelete); err != nil {
		t.Fatal(err)
	}
	if err := r.Run(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if !srv.keys["studio-ingest"].GetRevoked() || srv.keys["studio-ingest-r2"] == nil || srv.keys["studio-ingest-r2"].GetRevoked() {
		t.Errorf("keys: %v", srv.keys)
	}
	data, ann, _, _ := secrets.Get(ctx, "studio-evalsi-key")
	if string(data["api_key"]) != "evk_secret_of_studio-ingest-r2" || ann[KeyNameAnnotation] != "studio-ingest-r2" {
		t.Errorf("Secret: %v %v", data, ann)
	}
	// And once more: the replacement is kept.
	if err := client.CoreV1().Secrets("evalsi").Delete(ctx, "studio-evalsi-key", metaDelete); err != nil {
		t.Fatal(err)
	}
	if err := r.Run(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if !srv.keys["studio-ingest-r2"].GetRevoked() || srv.keys["studio-ingest-r3"] == nil {
		t.Errorf("keys: %v", srv.keys)
	}
}

func TestBootstrapKeepsAKeyWhoseRolesDiffer(t *testing.T) {
	cfg, _ := Load([]byte(sample))
	srv := newServer()
	r := runner(srv, KubeSecrets{Client: fake.NewSimpleClientset(), Namespace: "evalsi"})
	if err := r.Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	cfg.APIKeys[0].Roles = map[string][]string{"studio": {"runner"}}
	if err := r.Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if srv.calls["RevokeAPIKey"] != 0 || srv.calls["CreateAPIKey"] != 1 {
		t.Errorf("a key was rotated for a role change: %v", srv.calls)
	}
}

func TestBootstrapWaitsForTheServer(t *testing.T) {
	cfg, _ := Load([]byte(sample))
	srv := newServer()
	srv.whoamiFails = 3
	if err := runner(srv, KubeSecrets{Client: fake.NewSimpleClientset(), Namespace: "evalsi"}).Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	srv2 := newServer()
	srv2.whoamiFails = 1 << 30
	err := runner(srv2, KubeSecrets{Client: fake.NewSimpleClientset(), Namespace: "evalsi"}).Run(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "did not answer") {
		t.Errorf("a server that never answers: %v", err)
	}
}

func TestBootstrapFailsAtOnceWhenRefused(t *testing.T) {
	cfg, _ := Load([]byte(sample))
	srv := newServer()
	srv.whoamiErr = connect.NewError(connect.CodeUnauthenticated, errors.New("bad token"))
	r := runner(srv, KubeSecrets{Client: fake.NewSimpleClientset(), Namespace: "evalsi"})
	r.Wait = time.Hour
	start := time.Now()
	err := r.Run(context.Background(), cfg)
	if err == nil || time.Since(start) > 5*time.Second || !strings.Contains(err.Error(), "refused") {
		t.Errorf("err = %v after %s", err, time.Since(start))
	}
}

func TestValidate(t *testing.T) {
	for name, c := range map[string]string{
		"no key secret":  `{"api_keys":[{"name":"a","roles":{"p":["ingest"]}}]}`,
		"no roles":       `{"api_keys":[{"name":"a","secret":{"name":"s"}}]}`,
		"bad ttl":        `{"api_keys":[{"name":"a","roles":{"p":["x"]},"secret":{"name":"s"},"ttl":"soon"}]}`,
		"duplicate key":  `{"api_keys":[{"name":"a","roles":{"p":["x"]},"secret":{"name":"s"}},{"name":"a","roles":{"p":["x"]},"secret":{"name":"t"}}]}`,
		"bad event":      `{"webhooks":[{"name":"w","url":"https://x","events":["nope"]}]}`,
		"no project":     `{"projects":[{}]}`,
		"unknown field":  `{"project":[]}`,
		"bad policy":     `{"policies":[{"sampling":"lots"}]}`,
		"webhook no url": `{"webhooks":[{"name":"w"}]}`,
	} {
		if _, err := Load([]byte(c)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := Load([]byte(`{}`)); err != nil {
		t.Errorf("empty config: %v", err)
	}
}

var metaDelete = metav1.DeleteOptions{}
