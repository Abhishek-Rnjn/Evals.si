# Identity and access: setup guide

This guide sets up authentication and authorization on `evalsid`. The design and its reasoning are in [DESIGN §17](../DESIGN.md#17-security-identity-and-tenancy). A complete example config is in [`examples/auth/evalsi.yaml`](../../examples/auth/evalsi.yaml). To try it on a laptop first, with API keys only, follow "Try access control locally" in the [README](../../README.md#try-access-control-locally).

## How it fits together

- **Authentication** turns a credential into a principal: an OIDC/JWT bearer token, an API key (`evk_...`), a client certificate, or identity headers from a trusted proxy. Evals.si keeps no users or passwords.
- **Authorization** checks every call:
  - **Roles** are granted per project by bindings, by an API key's own roles, or by a token's role claims.
  - **Global CEL rules** (`deny`, `require`, `allow`) add install-wide restrictions on top.
  - **External authorization** (optional) defers to a central policy engine.
- **One gate covers every surface:** gRPC, gRPC-Web, Connect, the REST routes, OTLP (gRPC and HTTP), `/metrics` and reflection. `/healthz`, gRPC health and `/.well-known/evalsi-auth` stay open.

The server refuses to start on a non-loopback address without an `auth` section. Overrides such as `--listen` get the same check.

**Turning authentication off** (development, testing, demos):

- `evalsid serve --no-auth` (or `evalsi serve --no-auth`);
- `EVALSID_NO_AUTH=1`;
- `auth: {mode: none}` in the file.

Every caller is then treated as an owner, and a warning is logged on every start. Your `auth`, `rbac` and `authorization` sections stay in the config, unenforced, so switching back is just dropping the flag.

## 1. Start with API keys

API keys are the quickest way in, and the right credential for collectors and long-running services.

```bash
evalsid auth new-key
# key:  evk_3q2...           <- give this to the client
# hash: sha256:9f86d0...     <- put this in config
```

```yaml
listen: 0.0.0.0:8080
auth:
  api_keys:
    keys:
      - {name: admin,  key: "sha256:9f86d0...", roles: {"*": [owner]}}
      - {name: otel,   key: "sha256:2c26b4...", roles: {support: [ingest]}, labels: {source: gateway}}
  tls: {cert_file: /etc/evalsi/tls.crt, key_file: /etc/evalsi/tls.key}
rbac:
  projects:
    support: {}
```

Clients pass the key with `--api-key`, `EVALSI_API_KEY`, `Authorization: Bearer evk_...` or `X-Api-Key: evk_...`. Owners and project admins can also issue keys through the API, which stores only their hash and shows the key once:

```bash
evalsi auth keys create ci --role support=runner --ttl 90d --server https://evals.example.com
evalsi auth keys revoke ci
```

Bearer credentials over plaintext on a non-loopback address need `auth.tls`, or `auth.allow_plaintext: true` when TLS terminates in front of `evalsid`. Certificate and key files are reloaded when they change.

## 2. Add your identity provider (OIDC)

Each provider entry trusts one issuer:

```yaml
auth:
  jwt:
    mode: strict                 # strict (default) | optional | permissive
    providers:
      - name: corp               # bindings say user:corp/<sub>, group:corp/<group>
        issuer: https://sso.example.com/realms/eng
        audiences: [evals.si]    # required; tokens minted for other services are refused
        jwks: {discovery: true}  # or url:, file:, inline:
        claims: {groups: groups} # where subject, name, email and groups live
  cli_login: {issuer: https://sso.example.com/realms/eng, client_id: evalsi-cli}
```

Notes:

- Accepted algorithms default to RS256, ES256 and EdDSA; `none` is never accepted.
- `exp` is required by default (`required_claims`), with 60 seconds of clock skew allowed.
- Keys are cached and refreshed hourly. A token with an unknown `kid` triggers a refetch at most every 10 seconds.
- JWKS and discovery URLs must be `https://`. `jwks.allow_insecure` exists for local development only.
- The token is read from `Authorization: Bearer` by default; `location` can name another header, a cookie or a query parameter.

`cli_login` is what `evalsi login` uses. The CLI reads it from `/.well-known/evalsi-auth`, then signs in with the device code flow (default) or a browser with PKCE (`--browser`). The token it sends must carry an audience listed in `audiences`:

- an ID token's audience is the client ID;
- an API access token's audience is what your provider puts there.

Add whichever you use.

### Keycloak

```yaml
- name: kc
  issuer: https://keycloak.example.com/realms/eng
  audiences: [evalsi-cli, evals.si]
  jwks: {discovery: true}
  claims: {groups: groups}           # needs a "Group Membership" mapper (full path off)
  role_claims:                       # optional: realm roles become Evals.si roles
    claim: realm_access.roles
    map: {evals-admin: {"*": [admin]}}
```

Create a public client `evalsi-cli` with **OAuth 2.0 Device Authorization Grant** enabled (and standard flow with redirect `http://127.0.0.1/*` for `--browser`). Add an **Audience** mapper if you want `evals.si` in access tokens.

### Microsoft Entra ID

```yaml
- name: entra
  issuer: https://login.microsoftonline.com/<tenant-id>/v2.0
  audiences: [<application-client-id>, api://evals]
  jwks: {discovery: true}
  claims: {subject: oid, name: name, email: preferred_username, groups: groups}
  role_claims: {claim: roles, map: {Evals.Admin: {"*": [admin]}}}
```

- Use `oid` as the subject: `sub` is pairwise per application.
- Group claims carry object IDs (`group:entra/<object-id>`), and need **groups** configured in the token configuration.
- App roles arrive in `roles`.
- Enable **Allow public client flows** for the device code.

### Okta

```yaml
- name: okta
  issuer: https://<org>.okta.com/oauth2/default
  audiences: [api://default]
  jwks: {discovery: true}
  claims: {groups: groups}           # add a "groups" claim to the authorization server
```

Create a native app with the **Device Authorization** grant, and use access tokens (their audience is `api://default` unless you change it).

### Auth0

```yaml
- name: auth0
  issuer: https://<tenant>.us.auth0.com/   # keep the trailing slash: it is part of Auth0's iss
  audiences: [https://evals.example.com]   # the API identifier
  jwks: {discovery: true}
  claims: {groups: https://evals.example.com/groups}
  role_claims: {claim: https://evals.example.com/roles, map: {lead: {support: [editor]}}}
cli_login: {issuer: https://<tenant>.us.auth0.com/, client_id: <native-app-id>, audience: https://evals.example.com}
```

Auth0 adds custom claims only under a namespace; set them in a post-login Action. `cli_login.audience` makes Auth0 issue a JWT access token for the API.

### Google

```yaml
- name: google
  issuer: https://accounts.google.com
  audiences: [<oauth-client-id>.apps.googleusercontent.com]
  jwks: {discovery: true}
```

Google tokens carry no groups, so bind by verified email (`email:alice@example.com`), or by domain with a CEL subject (`cel:jwt.hd == "example.com"`). For `evalsi login`, create a **TVs and Limited Input** client for the device flow.

### Dex, and other providers

Any OIDC issuer works the same way. For Dex, request the `groups` scope (`cli_login.scopes: [openid, email, groups, offline_access]`).

## 3. Grant access: projects, roles, bindings

```yaml
rbac:
  owners: [group:corp/platform-admins]       # every permission, install-wide
  roles:
    - name: prompt-engineer                  # custom role
      inherits: [viewer]
      permissions: [evaluations.run, runs.create, runs.cancel]
      condition: 'resource.target.model in ["qwen3", "claude-opus-5-5"]'
  projects:
    support:
      admin:  [group:corp/support-leads]
      editor: [group:corp/support-eng]
      prompt-engineer: [group:corp/support-ml]
      viewer: ["email:auditor@example.com"]
      runner: ['cel:jwt.repository == "acme/support-agent" && jwt.ref == "refs/heads/main"']
```

**Built-in roles:**

| Role | Permissions |
|---|---|
| `viewer` | catalog, runs, policies, traces, annotation queues and guardrails: read |
| `runner` | viewer, plus evaluations, guardrail checks, and runs (create, cancel, resume) |
| `editor` | runner, plus writing policies and guardrails, promoting run results into datasets (`datasets.write`), and setting up and answering annotation queues |
| `annotator` | annotation queues: read and answer (`annotations.read`, `annotations.write`) |
| `admin` | editor, plus the project's keys, roles, bindings and audit log |
| `ingest` | `traces.write` only |
| `guard` | `guardrails.check` only: a gateway's credential for [inline guardrails](guardrails.md) |
| `owner` | everything, everywhere |

`evalsi auth roles permissions` lists every permission. Custom roles can also be managed through the API (`evalsi auth roles create|update|delete`) and bound with `evalsi auth bindings create`.

**Guardrails:**

- **No privilege escalation.** A project admin can only grant permissions it holds itself in that project, under conditions at least as strict as its own.
- **Validated up front.** Unknown permissions, inheritance cycles and conditions that do not compile are rejected.

**Subjects:**

| Subject | Matches |
|---|---|
| `user:<provider>/<sub>` | one principal |
| `group:<provider>/<group>` | members of a group |
| `email:<address>` | a verified email address |
| `key:<name>` | an API key |
| `cel:<expression>` | anything a CEL expression over `jwt`, `principal` and `source` can describe |

**Roles from the token.** If your provider already carries application roles, `role_claims` maps them per project, and your application keeps owning who has which role.

**Labels.** Runs (`CreateRun.labels`), policies, traces (OTLP resource attributes `evalsi.label.<key>`, plus the ingest key's labels) and API keys carry labels. Conditions and rules read them as `resource.labels`.

## 4. Global rules

```yaml
authorization:
  rules:
    - deny: 'request.action == "runs.create" && resource.target.connector == "anthropic" && !("llm-spenders" in principal.groups)'
    - require: '!resource.runs_code || "sandbox-users" in principal.groups'
    # Agent runs describe the agent: resource.agent.kind (builtin, a2a, mcp,
    # responses, http, cli), .harness (builtin, external), .image, .network
    # and .min_isolation.
    - deny: 'request.action == "runs.create" && resource.agent.network == "allow"'
```

**Decision order:**

1. A matching `deny` rule denies.
2. A `require` rule that does not hold denies.
3. A role that grants the action, with its condition holding, allows.
4. A matching `allow` rule allows.
5. Otherwise the request is denied.

A rule that fails to evaluate (for example, a missing attribute) counts as not matched. So restrictions belong in `require` rules, which then deny.

**MCP tools.** Calling a tool of the `/mcp` endpoint is the action `mcp.tools.call`, which any authenticated principal holds; rules restrict it per tool with `mcp.tool.name`, as agentgateway's `mcpAuthorization` does. Tools a principal may not call are left out of `tools/list`. Each tool still needs the permission of what it does (`evaluate` needs `evaluations.run`, `run` needs `runs.create`, and so on). Guard `require` rules on the action, since `mcp` is empty for other requests:

```yaml
authorization:
  rules:
    - deny: 'request.action == "mcp.tools.call" && mcp.tool.name == "run" && !("ci" in principal.groups)'
    - require: 'request.action != "mcp.tools.call" || mcp.tool.name in ["list_evaluators", "evaluate", "get_run", "compare_runs"]'
```

Explain any decision offline:

```bash
evalsid auth check --config evalsi.yaml --token "$TOKEN" --action runs.create --project support \
  --resource '{"target":{"connector":"anthropic","model":"claude-opus-5-5"},"runs_code":false}'
```

## 5. CI: GitHub Actions without stored secrets

Trust GitHub's issuer, and bind the repository and branch:

```yaml
auth:
  jwt:
    providers:
      - name: github
        issuer: https://token.actions.githubusercontent.com
        audiences: [https://evals.example.com]
        jwks: {discovery: true}
rbac:
  projects:
    support:
      runner: ['cel:jwt.repository == "acme/support-agent" && jwt.ref == "refs/heads/main"']
```

In the workflow, `EVALSI_OIDC_AUDIENCE` tells the CLI to fetch the job's OIDC token (see [`examples/ci/github-actions.yml`](../../examples/ci/github-actions.yml)):

```yaml
permissions: {id-token: write, contents: read}
steps:
  - run: evalsi run -f evals/run.yaml --server https://evals.example.com
    env: {EVALSI_OIDC_AUDIENCE: https://evals.example.com}
```

### On Kubernetes: service-account tokens

Workloads in the cluster (the operator, CI runners, agents) sign in with their own service account; no key to store or rotate. Trust the cluster's service-account issuer: `evalsid` reads its keys from the API server with its own token and CA, and discovers the issuer:

```yaml
auth:
  jwt:
    providers:
      - name: cluster
        audiences: [evals.si]            # tokens projected for evals.si only
        kubernetes:
          namespaces: [evalsi, ci]       # default: every namespace
rbac:
  projects:
    support:
      runner: ['group:cluster/system:serviceaccounts:ci']
  owners: ['user:cluster/system:serviceaccount:evalsi:evalsi-operator']
```

Some hardened clusters remove the `system:service-account-issuer-discovery` binding, so service accounts cannot read those keys. Then give `evalsid` the issuer and the key set yourself. Managed clusters publish both (EKS at `https://oidc.eks.<region>.amazonaws.com/id/<id>/keys`, GKE and AKS through the issuer's discovery document); otherwise a cluster admin can export them once (`kubectl get --raw /openid/v1/jwks`), and must again when the cluster rotates its keys. The API server is then never called, and no token is sent anywhere:

```yaml
      - name: cluster
        audiences: [evals.si]
        issuer: https://oidc.eks.eu-west-1.amazonaws.com/id/EXAMPLE   # the iss of the cluster's tokens
        jwks: {url: https://oidc.eks.eu-west-1.amazonaws.com/id/EXAMPLE/keys}   # or {file: ...}, {inline: ...}, {discovery: true}
        kubernetes: {namespaces: [evalsi, ci]}
```

In the Helm chart these are `auth.kubernetes.issuer` with `auth.kubernetes.jwks` or `auth.kubernetes.jwksConfigMap` (a ConfigMap holding `keys.json`).

A service account becomes `system:serviceaccount:<namespace>:<name>` (kind `service`), in the groups `system:serviceaccounts` and `system:serviceaccounts:<namespace>`. Mount a token for the `evals.si` audience and point the CLI at it; it is read on every request, so the kubelet's rotation needs nothing:

```yaml
volumes:
  - name: evalsi-token
    projected:
      sources: [{serviceAccountToken: {audience: evals.si, expirationSeconds: 3600, path: token}}]
env:
  - {name: EVALSI_TOKEN_FILE, value: /var/run/secrets/evalsi/token}
```

### Between components: mutual TLS

Every hop can run over mutual TLS with certificates from your CA (cert-manager, for example), reloaded when they rotate:

| Hop | Configure |
|---|---|
| clients to `evalsid` | `auth.tls` (`client_ca`, `require_client_cert`); a verified certificate's SPIFFE ID or common name is the principal |
| operator to `evalsid` | `--ca-file`, `--cert-file`, `--key-file` |
| `evalsid` and workers to NATS | `cluster.tls` |
| workers to sandbox pools | `worker.sandbox_service` (`ca_file`, `cert_file`, `key_file`); pools accept only the clients named by `--allow-client` |
| to ClickHouse and S3 | `storage.clickhouse.tls`, `storage.s3.tls` (`ca_file`, `cert_file`, `key_file`, `server_name`) |
| to PostgreSQL | the DSN: `sslmode=verify-full sslrootcert=... sslcert=... sslkey=...` |

## 6. Behind agentgateway

- **Forward the token.** Let agentgateway validate the JWT and forward it (`preserveToken`); `evalsid` validates the same token again against the same issuer. Trusting identity headers instead (`auth.trusted_proxy`, limited to configured CIDRs) is supported but discouraged.
- **Ingest with a key.** Point agentgateway's OTLP export at `evalsid` with an `ingest` key bound to one project, sent as a header. Traces land in that project, and their labels come from the key. A resource attribute `evalsi.project` is honored only when the key may write there.

## 7. Audit, metrics, external authorization

- **Audit.** Mutating calls and every denied call are written to the audit log, with the deciding rule or role. Read it with `evalsi auth audit [--denied]`; it is kept for `audit.retention` (default 90 days). Export it to your log pipeline with an OTel sink and `audit: true`.
- **Metrics.** `/metrics` on the main port needs `metrics.read` (owners). Or serve it unauthenticated on a loopback listener with `metrics: {listen: 127.0.0.1:9464}`.
- **External authorization.** A central policy engine can decide too:

  ```yaml
  authorization:
    ext_authz:
      authzen: {url: https://pdp.example.com/access/v1/evaluation, token_env: PDP_TOKEN}
      # or envoy: {address: opa.internal:9191}   (Envoy ext_authz gRPC, which OPA serves)
      mode: require      # require: both must allow | decide: the PDP decides after deny/require rules
      timeout: 200ms     # timeouts and errors deny
      cache_ttl: 30s
  ```

## 8. Credentials: which worker secrets a project may use

Run specs, agents and evaluator params name secrets by variable, and the worker reads them from its own environment:

- `target.api_key_env`;
- an agent's `headers_env` or `api_key_env`, and a CLI agent's `env_from`;
- an MCP server's `headers_env`;
- evaluator params an evaluator declares as secrets, and any evaluator or external-harness param ending in `_env` (for example `builtin/reward-model`'s `api_key_env`).

The worker sends each value to the URL the same spec names. Without a check, anyone who may create a run in any project could have the worker send any variable it holds (a database DSN, another team's key) to a host of their choosing. So variables are granted per project, and a grant can limit where its value may go:

```yaml
credentials:
  grants:
    - env: OPENAI_API_KEY
      projects: ["*"]                     # every project
      hosts: [api.openai.com]             # and only to OpenAI, over HTTPS
    - env: SUPPORT_AZURE_KEY
      projects: [support]
      hosts: ["*.openai.azure.com"]       # any subdomain, not the domain itself
    - env: LAB_VLLM_KEY
      projects: [research]
      hosts: [vllm.lab.internal]
      allow_http: true                    # a model server without TLS on a trusted network
    - env: SUPPORT_AGENT_TOKEN            # no hosts: any destination
      projects: [support]
judges:
  claude:
    provider: anthropic
    model: claude-opus-5-5
    projects: [support, research]         # only these projects may use this judge (default: all)
```

- **When it applies:** grant checks are on whenever authentication is enabled or any grant is listed. A request naming a variable its project has no grant for, or sending a host-limited one elsewhere, is refused with `permission_denied` before anything is stored or run. `credentials: {enforce: false}` turns grant checks off; that suits only a single-user server. Judge `projects` apply either way.
- **Defaults count:** a target without `api_key_env` still sends the connector's default key (`OPENAI_API_KEY` for openai-compatible, `ANTHROPIC_API_KEY` for anthropic), so that default needs a grant too. A target that needs no key (a local model server) says `api_key_env: none`.
- **Hosts and HTTPS:** hosts are matched against the host of the URL the value goes to: the target's `base_url` (`api.anthropic.com` for anthropic without one), the agent's or MCP server's `url`, or the param an evaluator declares as the destination (`url`/`base_url` beside an `_env` param). A host-limited value goes only over HTTPS, except to loopback or with `allow_http`. A host-limited variable named where no URL is given is refused.
- **Sandboxes:** a CLI agent's `env_from` copies the value into a sandbox, whose traffic evalsid does not see request by request. Only grants without `hosts` can be used there; limit the sandbox's network instead (`environment.sandbox.network: allowlist`).
- **Judges:** a judge with `projects` is open to those projects only, wherever a judge is used: evaluators that need one, the default judge, and a user simulator. Its key and budget are the server's, so this keeps a costly judge to the teams that pay for it.
- **Where it is checked:** creating runs and shadow replays, `Evaluate` and `EvaluateStream`, reward scoring, online policies and guardrails (stored and inline). With Kubernetes, the operator's admission webhook asks evalsid the same question (`CreateRun`/`ApplyPolicy` with `validate_only`), so `kubectl apply` of an `EvalRun` or `OnlineEvalPolicy` naming an ungranted variable fails at once instead of in the resource's status. If evalsid cannot answer within 5 seconds, or the project does not exist yet, the resource is admitted with a warning and the controller reports the outcome.
- **Audit and metrics:** every refusal is written to the audit log as action `credentials.use`, resource `env:NAME` or `judge:NAME`, with the caller and the reason (`evalsi auth audit --denied`), and counted in `evalsi_credential_denied_total{project,kind,name}` on `/metrics`.
- **What a project may use:** `evalsi credentials list --server URL --project P` (or `GET /v1alpha1/credentials?project=P`, or the web UI's catalog page) lists the grants that apply to a project (names and hosts, never values) and the judges it may use. It needs `runs.read` in the project.
- **Changing grants:** evalsid reads the config file again every 15 seconds and on `SIGHUP`, and applies changed `credentials` and judge `projects` without a restart (other settings still need one). Requests are checked against the new grants at once; online policies are checked again, and one that names what its project may no longer use stops until the grant returns or the policy changes; guardrails are checked again on their next use. Runs already started keep running. In Kubernetes, `helm upgrade` with new `server.config.credentials` rolls the replicas onto the new config as usual; an `evalsi` ConfigMap edited in place reaches running replicas without a restart, within the kubelet's sync period.
- **What it does not cover:** judges' own keys (`judges.*.api_key_env`) and sinks come from the server config, which is trusted. Grants scope variable names, not values: give each project that needs its own key its own variable.

### Evaluators that read secrets

An evaluator that reads a worker variable named by a param must declare that param, so evalsid can check it against the grants:

```python
@evaluator(name="acme/scored", version="1.0.0", secrets={"token_var": "endpoint"})
def scored(record, *, endpoint: str, token_var: str = "") -> Score:
    ...
```

`secrets` maps each such param to the param holding the URL its value is sent to (`""` when there is none, which a host-limited grant refuses). Params ending in `_env` are declared automatically, sent to `url` or `base_url` when the evaluator has one. The declaration travels in the manifest's params schema (`"x-evalsi-secret": {"sent_to": ...}`). As a backstop, the worker refuses any call in which an undeclared param's value is the name of one of its environment variables, so an evaluator cannot read a secret through a param it did not declare.
