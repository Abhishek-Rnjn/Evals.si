package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/abhishek-rnjn/evals.si/internal/auth"
	"github.com/abhishek-rnjn/evals.si/internal/authz"
	"github.com/abhishek-rnjn/evals.si/internal/config"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

const authUsage = `usage: evalsid auth <command> [flags]

commands:
  check      explain an authorization decision for a token or API key, offline
  new-key    generate an API key and the sha256: hash to put in config
  hash-key   print the sha256: hash of a key read from stdin
`

func authMain(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, authUsage)
		return 2
	}
	switch args[0] {
	case "check":
		return authCheck(ctx, args[1:], stdout, stderr)
	case "new-key":
		key, hash := auth.NewAPIKey()
		fmt.Fprintf(stdout, "key:  %s\nhash: sha256:%s\n\nPut the hash in auth.api_keys.keys[].key and give the key to the client; it is not stored anywhere.\n", key, hash)
		return 0
	case "hash-key":
		line, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			fmt.Fprintf(stderr, "evalsid: %v\n", err)
			return 1
		}
		key := strings.TrimSpace(line)
		if !strings.HasPrefix(key, auth.APIKeyPrefix) {
			fmt.Fprintf(stderr, "evalsid: API keys start with %q\n", auth.APIKeyPrefix)
			return 1
		}
		fmt.Fprintf(stdout, "sha256:%s\n", auth.HashAPIKey(key))
		return 0
	default:
		fmt.Fprintf(stderr, "evalsid: unknown auth command %q\n\n%s", args[0], authUsage)
		return 2
	}
}

func authCheck(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("auth check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "path to evalsi.yaml")
	token := fs.String("token", "", "a JWT (or - to read it from stdin)")
	apiKey := fs.String("api-key", "", "an API key (or - to read it from stdin)")
	action := fs.String("action", "", "the permission, for example runs.create")
	project := fs.String("project", "default", "the project")
	resource := fs.String("resource", "{}", `resource attributes as JSON, for example '{"target":{"model":"qwen3"},"runs_code":false}'`)
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "evalsid: %v\n", err)
		return 1
	}
	if *action == "" {
		return fail(errors.New("--action is required"))
	}
	for _, p := range []*string{token, apiKey} {
		if *p == "-" {
			raw, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<16))
			if err != nil {
				return fail(err)
			}
			*p = strings.TrimSpace(string(raw))
		}
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(*resource), &res); err != nil {
		return fail(fmt.Errorf("--resource: %w", err))
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return fail(err)
	}
	// Roles, bindings and keys created through the API live in the database;
	// read them when it exists, never create one.
	var st *store.Store
	if db := filepath.Join(cfg.DataDir, "evalsi.db"); fileExists(db) {
		if st, err = store.Open(db); err != nil {
			return fail(err)
		}
		defer st.Close()
	}
	var keys auth.KeyStore
	var authzStore authz.Store
	if st != nil {
		keys, authzStore = st, st
	}
	authn, err := auth.New(cfg.Auth, keys, nil, nil)
	if err != nil {
		return fail(err)
	}
	engine, err := authz.NewEngine(ctx, authz.Options{
		Enabled: cfg.AuthEnabled(), RBAC: cfg.RBAC, Authorization: cfg.Authorization, Store: authzStore,
	})
	if err != nil {
		return fail(err)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/", nil)
	req.RemoteAddr = "127.0.0.1:0"
	switch {
	case *apiKey != "":
		req.Header.Set(auth.APIKeyHeader, *apiKey)
	case *token != "":
		req.Header.Set("Authorization", "Bearer "+*token)
	}
	result := authn.Authenticate(req)
	if result.Err != nil {
		if *asJSON {
			_ = json.NewEncoder(stdout).Encode(map[string]any{"authenticated": false, "error": result.Err.Error()})
		} else {
			fmt.Fprintf(stdout, "authentication: FAILED (%v)\ndecision:       deny\n", result.Err)
		}
		return 3
	}
	p := result.Principal
	ex := engine.Explain(ctx, p, authz.Request{Action: *action, Project: *project, Resource: res, Procedure: "evalsid auth check", Protocol: "cli", Source: req.RemoteAddr})
	if *asJSON {
		_ = json.NewEncoder(stdout).Encode(map[string]any{
			"authenticated": true, "principal": p.ID(), "kind": p.Kind, "groups": p.Groups,
			"roles": ex.Roles, "rules": ex.Rules, "allowed": ex.Decision.Allowed, "reason": ex.Decision.Reason,
		})
	} else {
		fmt.Fprintf(stdout, "principal: %s (%s via %s)\n", p.ID(), p.Kind, p.Method)
		if len(p.Groups) > 0 {
			fmt.Fprintf(stdout, "groups:    %s\n", strings.Join(p.Groups, ", "))
		}
		fmt.Fprintf(stdout, "request:   %s in project %q\n\nroles:\n", *action, *project)
		if len(ex.Roles) == 0 {
			fmt.Fprintln(stdout, "  (none)")
		}
		for _, r := range ex.Roles {
			fmt.Fprintf(stdout, "  %-20s in %-12s via %-40s grants %s: %v\n", r.Role, r.Scope, r.Via, *action, r.Grants)
		}
		fmt.Fprintln(stdout, "\nrules:")
		if len(ex.Rules) == 0 {
			fmt.Fprintln(stdout, "  (none)")
		}
		for i, r := range ex.Rules {
			fmt.Fprintf(stdout, "  %d %-7s %-14s %s\n", i, r.Kind, r.Result, r.Expr)
		}
		verdict := "deny"
		if ex.Decision.Allowed {
			verdict = "allow"
		}
		fmt.Fprintf(stdout, "\ndecision:  %s (%s)\n", verdict, ex.Decision.Reason)
	}
	if !ex.Decision.Allowed {
		return 3
	}
	return 0
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
