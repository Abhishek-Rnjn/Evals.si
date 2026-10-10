// Package bootstrap makes the first state of an install from the Helm
// chart's values: projects, API keys (written to Secrets), default policies
// and webhooks. It runs as a Job after each install and upgrade, so it is
// idempotent: what exists is left alone, what is missing is made, and a
// key's Secret is written once and then kept.
package bootstrap

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/durationpb"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/operator/controllers"
)

// Config is the bootstrap file the chart renders from its values.
type Config struct {
	Projects []Project         `json:"projects,omitempty"`
	APIKeys  []APIKey          `json:"api_keys,omitempty"`
	Policies []json.RawMessage `json:"policies,omitempty"`
	Webhooks []Webhook         `json:"webhooks,omitempty"`
}

// Project to create.
type Project struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// SecretRef names where a generated secret value is kept.
type SecretRef struct {
	Name string `json:"name"`
	// Default: api_key for API keys, secret for webhooks.
	Key string `json:"key,omitempty"`
}

// APIKey to create, and the Secret that holds its value.
type APIKey struct {
	Name string `json:"name"`
	// Roles per project ("*" for every project).
	Roles  map[string][]string `json:"roles"`
	Labels map[string]string   `json:"labels,omitempty"`
	// A lifetime such as 720h; none: no expiry.
	TTL    string    `json:"ttl,omitempty"`
	Secret SecretRef `json:"secret"`
}

// Webhook to apply. Its HMAC secret is generated here and kept in Secret
// (optional: without one the server generates it and it is not kept).
type Webhook struct {
	Name     string            `json:"name"`
	Project  string            `json:"project,omitempty"`
	URL      string            `json:"url"`
	Events   []string          `json:"events,omitempty"`
	Disabled bool              `json:"disabled,omitempty"`
	Labels   map[string]string `json:"labels,omitempty"`
	Secret   SecretRef         `json:"secret,omitempty"`
}

// Validate checks a config before anything is created.
func (c Config) Validate() error {
	var errs []error
	seen := map[string]bool{}
	for i, p := range c.Projects {
		if p.Name == "" {
			errs = append(errs, fmt.Errorf("projects[%d]: name is required", i))
		}
	}
	for i, k := range c.APIKeys {
		switch {
		case k.Name == "":
			errs = append(errs, fmt.Errorf("api_keys[%d]: name is required", i))
		case len(k.Roles) == 0:
			errs = append(errs, fmt.Errorf("api_keys[%s]: roles are required (project -> roles)", k.Name))
		case k.Secret.Name == "":
			errs = append(errs, fmt.Errorf("api_keys[%s]: secret.name is required: the key is shown once and must be kept somewhere", k.Name))
		}
		if k.TTL != "" {
			if d, err := time.ParseDuration(k.TTL); err != nil || d <= 0 {
				errs = append(errs, fmt.Errorf("api_keys[%s]: ttl must be a positive duration such as 720h", k.Name))
			}
		}
		if seen["key/"+k.Name] {
			errs = append(errs, fmt.Errorf("api_keys[%s]: listed twice", k.Name))
		}
		seen["key/"+k.Name] = true
	}
	for i, w := range c.Webhooks {
		if w.Name == "" || w.URL == "" {
			errs = append(errs, fmt.Errorf("webhooks[%d]: name and url are required", i))
		}
		for _, e := range w.Events {
			if _, ok := events[e]; !ok {
				errs = append(errs, fmt.Errorf("webhooks[%s]: unknown event %q (run.finished, run.gate_failed, trace.scored, alert.fired or alert.resolved)", w.Name, e))
			}
		}
	}
	for i, raw := range c.Policies {
		if err := protojson.Unmarshal(raw, &evalsiv1alpha1.OnlineEvalPolicy{}); err != nil {
			errs = append(errs, fmt.Errorf("policies[%d]: %w", i, err))
		}
	}
	return errors.Join(errs...)
}

var events = map[string]evalsiv1alpha1.WebhookEvent{
	"run.finished":    evalsiv1alpha1.WebhookEvent_WEBHOOK_EVENT_RUN_FINISHED,
	"run.gate_failed": evalsiv1alpha1.WebhookEvent_WEBHOOK_EVENT_RUN_GATE_FAILED,
	"trace.scored":    evalsiv1alpha1.WebhookEvent_WEBHOOK_EVENT_TRACE_SCORED,
	"alert.fired":     evalsiv1alpha1.WebhookEvent_WEBHOOK_EVENT_ALERT_FIRED,
	"alert.resolved":  evalsiv1alpha1.WebhookEvent_WEBHOOK_EVENT_ALERT_RESOLVED,
}

// Secrets keeps the values Bootstrap generates (Kubernetes Secrets).
type Secrets interface {
	// Get returns a Secret's data and annotations; found is false when it does not exist.
	Get(ctx context.Context, name string) (data map[string][]byte, annotations map[string]string, found bool, err error)
	// Put creates or replaces a Secret.
	Put(ctx context.Context, name string, data map[string][]byte, annotations map[string]string) error
}

// Runner applies a Config.
type Runner struct {
	API     *controllers.API
	Secrets Secrets
	Log     *slog.Logger
	// How long to wait for the server to accept calls (default 10 minutes).
	Wait time.Duration
	// Sleep between attempts to reach the server (default 3s).
	Poll time.Duration
}

// KeyNameAnnotation records on a key's Secret which API key its value belongs to.
const KeyNameAnnotation = "evals.si/api-key-name"

// Run creates what cfg describes and is missing.
func (r *Runner) Run(ctx context.Context, cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if r.Log == nil {
		r.Log = slog.Default()
	}
	if err := r.waitForServer(ctx); err != nil {
		return err
	}
	var errs []error
	for _, p := range cfg.Projects {
		errs = append(errs, r.project(ctx, p))
	}
	for _, k := range cfg.APIKeys {
		errs = append(errs, r.apiKey(ctx, k))
	}
	for i, raw := range cfg.Policies {
		errs = append(errs, r.policy(ctx, i, raw))
	}
	for _, w := range cfg.Webhooks {
		errs = append(errs, r.webhook(ctx, w))
	}
	return errors.Join(errs...)
}

func (r *Runner) waitForServer(ctx context.Context) error {
	wait, poll := r.Wait, r.Poll
	if wait <= 0 {
		wait = 10 * time.Minute
	}
	if poll <= 0 {
		poll = 3 * time.Second
	}
	deadline := time.Now().Add(wait)
	for {
		_, err := r.API.Auth.WhoAmI(ctx, connect.NewRequest(&evalsiv1alpha1.WhoAmIRequest{}))
		if err == nil {
			return nil
		}
		// An answer that says no is final: waiting does not change it.
		if c := connect.CodeOf(err); c == connect.CodePermissionDenied || c == connect.CodeUnauthenticated {
			return fmt.Errorf("the server refused the bootstrap Job's credential: %w", err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the server did not answer within %s: %w", wait, err)
		}
		r.Log.Info("waiting for the server", "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

func (r *Runner) project(ctx context.Context, p Project) error {
	_, err := r.API.Auth.CreateProject(ctx, connect.NewRequest(&evalsiv1alpha1.CreateProjectRequest{Name: p.Name, Description: p.Description}))
	switch {
	case err == nil:
		r.Log.Info("created project", "project", p.Name)
	case connect.CodeOf(err) == connect.CodeAlreadyExists:
		r.Log.Info("project exists", "project", p.Name)
	default:
		return fmt.Errorf("project %s: %w", p.Name, err)
	}
	return nil
}

func rolesProto(roles map[string][]string) map[string]*evalsiv1alpha1.RoleList {
	out := map[string]*evalsiv1alpha1.RoleList{}
	for p, rs := range roles {
		out[p] = &evalsiv1alpha1.RoleList{Roles: rs}
	}
	return out
}

func sameRoles(have map[string]*evalsiv1alpha1.RoleList, want map[string][]string) bool {
	if len(have) != len(want) {
		return false
	}
	for p, rs := range want {
		h := slices.Clone(have[p].GetRoles())
		w := slices.Clone(rs)
		slices.Sort(h)
		slices.Sort(w)
		if !slices.Equal(h, w) {
			return false
		}
	}
	return true
}

// apiKey makes sure the key exists and its value is in its Secret. A key's
// value is shown once, so a key whose Secret is gone is replaced by a new one
// (the old one is revoked), named <name>-r2, -r3...: revoked names stay taken.
func (r *Runner) apiKey(ctx context.Context, k APIKey) error {
	field := k.Secret.Key
	if field == "" {
		field = "api_key"
	}
	listed, err := r.API.Auth.ListAPIKeys(ctx, connect.NewRequest(&evalsiv1alpha1.ListAPIKeysRequest{}))
	if err != nil {
		return fmt.Errorf("api key %s: %w", k.Name, err)
	}
	byName := map[string]*evalsiv1alpha1.APIKey{}
	for _, key := range listed.Msg.GetKeys() {
		byName[key.GetName()] = key
	}
	data, ann, found, err := r.Secrets.Get(ctx, k.Secret.Name)
	if err != nil {
		return fmt.Errorf("api key %s: reading Secret %s: %w", k.Name, k.Secret.Name, err)
	}
	if found && len(data[field]) > 0 {
		have := byName[ann[KeyNameAnnotation]]
		if have != nil && !have.GetRevoked() {
			if !sameRoles(have.GetRoles(), k.Roles) {
				r.Log.Warn("the key's roles differ from the values, and a key's roles cannot change; rename the key in the values to replace it",
					"key", have.GetName())
			}
			r.Log.Info("api key exists", "key", have.GetName(), "secret", k.Secret.Name)
			return nil
		}
	}
	// Choose a name that is free: the configured one first.
	name := k.Name
	for n := 2; byName[name] != nil; n++ {
		if old := byName[name]; !old.GetRevoked() {
			if _, err := r.API.Auth.RevokeAPIKey(ctx, connect.NewRequest(&evalsiv1alpha1.RevokeAPIKeyRequest{Name: name})); err != nil {
				return fmt.Errorf("api key %s: revoking %s, whose Secret is gone: %w", k.Name, name, err)
			}
			r.Log.Info("revoked a key whose Secret is gone", "key", name)
		}
		name = k.Name + "-r" + strconv.Itoa(n)
	}
	req := &evalsiv1alpha1.CreateAPIKeyRequest{Name: name, Roles: rolesProto(k.Roles), Labels: k.Labels}
	if k.TTL != "" {
		d, _ := time.ParseDuration(k.TTL)
		req.Ttl = durationpb.New(d)
	}
	resp, err := r.API.Auth.CreateAPIKey(ctx, connect.NewRequest(req))
	if err != nil {
		return fmt.Errorf("api key %s: %w", name, err)
	}
	if err := r.Secrets.Put(ctx, k.Secret.Name, map[string][]byte{field: []byte(resp.Msg.GetSecret())}, map[string]string{KeyNameAnnotation: name}); err != nil {
		// The key exists but its value is lost; revoke it so a rerun starts clean.
		_, rerr := r.API.Auth.RevokeAPIKey(ctx, connect.NewRequest(&evalsiv1alpha1.RevokeAPIKeyRequest{Name: name}))
		return fmt.Errorf("api key %s: writing Secret %s: %w (revoking the key: %v)", name, k.Secret.Name, err, rerr)
	}
	r.Log.Info("created api key", "key", name, "secret", k.Secret.Name)
	return nil
}

func (r *Runner) policy(ctx context.Context, i int, raw json.RawMessage) error {
	p := &evalsiv1alpha1.OnlineEvalPolicy{}
	if err := protojson.Unmarshal(raw, p); err != nil {
		return fmt.Errorf("policies[%d]: %w", i, err)
	}
	if _, err := r.API.Monitor.ApplyPolicy(ctx, connect.NewRequest(&evalsiv1alpha1.ApplyPolicyRequest{Policy: p})); err != nil {
		return fmt.Errorf("policy %s: %w", p.GetName(), err)
	}
	r.Log.Info("applied policy", "policy", p.GetName())
	return nil
}

func newSecret() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return "whsec_" + base64.RawURLEncoding.EncodeToString(b)
}

func (r *Runner) webhook(ctx context.Context, w Webhook) error {
	hook := &evalsiv1alpha1.Webhook{Name: w.Name, Project: w.Project, Url: w.URL, Disabled: w.Disabled, Labels: w.Labels}
	for _, e := range w.Events {
		hook.Events = append(hook.Events, events[e])
	}
	field := w.Secret.Key
	if field == "" {
		field = "secret"
	}
	var data map[string][]byte
	if w.Secret.Name != "" {
		d, _, found, err := r.Secrets.Get(ctx, w.Secret.Name)
		if err != nil {
			return fmt.Errorf("webhook %s: reading Secret %s: %w", w.Name, w.Secret.Name, err)
		}
		if found && len(d[field]) > 0 {
			hook.Secret = string(d[field])
		} else {
			hook.Secret = newSecret()
			data = map[string][]byte{field: []byte(hook.Secret)}
		}
	}
	// The Secret is written first: a webhook whose secret nobody holds is
	// worse than a Secret for a webhook not yet applied.
	if data != nil {
		if err := r.Secrets.Put(ctx, w.Secret.Name, data, nil); err != nil {
			return fmt.Errorf("webhook %s: writing Secret %s: %w", w.Name, w.Secret.Name, err)
		}
	}
	if _, err := r.API.Webhooks.ApplyWebhook(ctx, connect.NewRequest(&evalsiv1alpha1.ApplyWebhookRequest{Webhook: hook})); err != nil {
		return fmt.Errorf("webhook %s: %w", w.Name, err)
	}
	r.Log.Info("applied webhook", "webhook", w.Name)
	return nil
}

// Load parses a bootstrap file.
func Load(raw []byte) (Config, error) {
	var c Config
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("bootstrap config: %w", err)
	}
	return c, c.Validate()
}
