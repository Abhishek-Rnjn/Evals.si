"""Credentials for evalsid servers: API keys, tokens, ``evalsi login`` and CI.

How a client finds its credential, first match wins:

1. ``api_key=`` or ``token=`` passed explicitly (``--api-key`` / ``--token``);
2. the ``EVALSI_API_KEY`` or ``EVALSI_TOKEN`` environment variable;
3. in GitHub Actions, the job's own OIDC token, when ``EVALSI_OIDC_AUDIENCE``
   names the audience the server expects (no stored secret);
4. the token cached by ``evalsi login`` for that server, refreshed when it
   is about to expire.

``evalsi login`` signs in with the OAuth 2.0 device authorization grant
(RFC 8628), or the authorization code flow with PKCE and a loopback redirect
(RFC 7636, RFC 8252). The server publishes the issuer and client ID to use at
``/.well-known/evalsi-auth``. Tokens are cached in
``~/.config/evalsi/credentials`` (mode 0600), keyed by server URL.
"""

from __future__ import annotations

import base64
import hashlib
import http.server as http_server
import json
import os
import secrets
import threading
import time
import urllib.parse
import webbrowser
from collections.abc import Callable, Generator
from dataclasses import asdict, dataclass, field
from pathlib import Path
from typing import Any

import httpx

API_KEY_PREFIX = "evk_"
# Refresh tokens this long before they expire.
REFRESH_MARGIN_S = 60.0


class AuthError(RuntimeError):
    """Signing in or finding a credential failed."""


@dataclass
class Credentials:
    """A cached login for one server."""

    access_token: str = ""
    id_token: str = ""
    refresh_token: str = ""
    expires_at: float = 0.0
    token_endpoint: str = ""
    client_id: str = ""
    issuer: str = ""
    scopes: list[str] = field(default_factory=list)

    def bearer(self) -> str:
        """The token evalsid validates: the access token when it is a JWT
        (an audience-bound API token), otherwise the ID token."""
        if self.access_token.count(".") == 2:
            return self.access_token
        return self.id_token or self.access_token

    def expired(self, now: float | None = None) -> bool:
        now = time.time() if now is None else now
        exp = self.expires_at or jwt_expiry(self.bearer()) or 0.0
        return bool(exp) and now >= exp - REFRESH_MARGIN_S


def jwt_expiry(token: str) -> float | None:
    """The exp claim of a JWT, read without verification (only to know when to refresh)."""
    parts = token.split(".")
    if len(parts) != 3:
        return None
    try:
        payload = json.loads(base64.urlsafe_b64decode(parts[1] + "=" * (-len(parts[1]) % 4)))
    except ValueError:
        return None
    exp = payload.get("exp") if isinstance(payload, dict) else None
    return float(exp) if isinstance(exp, int | float) else None


# --- the credential cache ---


def credentials_path() -> Path:
    if path := os.environ.get("EVALSI_CREDENTIALS"):
        return Path(path)
    base = os.environ.get("XDG_CONFIG_HOME") or str(Path.home() / ".config")
    return Path(base) / "evalsi" / "credentials"


def _server_key(server: str) -> str:
    return server.rstrip("/")


def _load_all() -> dict[str, Any]:
    path = credentials_path()
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except FileNotFoundError:
        return {}
    except (OSError, ValueError) as exc:
        raise AuthError(f"reading {path}: {exc}") from exc
    servers = data.get("servers") if isinstance(data, dict) else None
    return servers if isinstance(servers, dict) else {}


def _save_all(servers: dict[str, Any]) -> None:
    path = credentials_path()
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    tmp = path.with_name(path.name + f".{os.getpid()}.tmp")
    # Created 0600 from the start: tokens are never world-readable, even briefly.
    fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w", encoding="utf-8") as handle:
        json.dump({"servers": servers}, handle, indent=2)
    os.replace(tmp, path)
    os.chmod(path, 0o600)


def load_credentials(server: str) -> Credentials | None:
    entry = _load_all().get(_server_key(server))
    if not isinstance(entry, dict):
        return None
    known = {k: v for k, v in entry.items() if k in Credentials.__dataclass_fields__}
    return Credentials(**known)


def save_credentials(server: str, creds: Credentials) -> None:
    servers = _load_all()
    servers[_server_key(server)] = asdict(creds)
    _save_all(servers)


def delete_credentials(server: str) -> bool:
    servers = _load_all()
    if servers.pop(_server_key(server), None) is None:
        return False
    _save_all(servers)
    return True


# --- discovery ---


@dataclass
class LoginConfig:
    issuer: str
    client_id: str
    scopes: list[str]
    audience: str
    token_endpoint: str
    device_endpoint: str
    authorization_endpoint: str


def discover(server: str, http: httpx.Client) -> LoginConfig:
    """Read the server's login settings, then its issuer's OIDC metadata."""
    resp = http.get(_server_key(server) + "/.well-known/evalsi-auth")
    resp.raise_for_status()
    doc = resp.json()
    if not doc.get("issuer") or not doc.get("client_id"):
        raise AuthError(
            "this server does not advertise a login provider (auth.cli_login); "
            "use an API key or EVALSI_TOKEN instead"
        )
    issuer = str(doc["issuer"]).rstrip("/")
    meta_resp = http.get(issuer + "/.well-known/openid-configuration")
    meta_resp.raise_for_status()
    meta = meta_resp.json()
    scopes = list(doc.get("scopes") or []) or ["openid", "profile", "email", "offline_access"]
    return LoginConfig(
        issuer=issuer,
        client_id=str(doc["client_id"]),
        scopes=scopes,
        audience=str(doc.get("audience") or ""),
        token_endpoint=str(meta.get("token_endpoint", "")),
        device_endpoint=str(meta.get("device_authorization_endpoint", "")),
        authorization_endpoint=str(meta.get("authorization_endpoint", "")),
    )


def _token_response(cfg: LoginConfig, body: dict[str, Any]) -> Credentials:
    if "error" in body:
        raise AuthError(f"{body['error']}: {body.get('error_description', '')}".rstrip(": "))
    expires_in = body.get("expires_in")
    return Credentials(
        access_token=str(body.get("access_token", "")),
        id_token=str(body.get("id_token", "")),
        refresh_token=str(body.get("refresh_token", "")),
        expires_at=time.time() + float(expires_in) if expires_in else 0.0,
        token_endpoint=cfg.token_endpoint,
        client_id=cfg.client_id,
        issuer=cfg.issuer,
        scopes=cfg.scopes,
    )


# --- device authorization grant (RFC 8628) ---


def device_login(
    cfg: LoginConfig,
    http: httpx.Client,
    *,
    show: Callable[[str, str, str], None],
    sleep: Callable[[float], None] = time.sleep,
) -> Credentials:
    """Ask the provider for a user code, show it, and poll until approved.

    ``show`` gets the verification URI, the user code and the complete URI.
    """
    if not cfg.device_endpoint:
        raise AuthError("the identity provider has no device authorization endpoint; use --browser")
    form = {"client_id": cfg.client_id, "scope": " ".join(cfg.scopes)}
    if cfg.audience:
        form["audience"] = cfg.audience
    resp = http.post(cfg.device_endpoint, data=form)
    start = resp.json()
    if resp.status_code != 200 or "device_code" not in start:
        raise AuthError(f"device authorization failed: {start}")
    show(
        str(start.get("verification_uri", "")),
        str(start.get("user_code", "")),
        str(start.get("verification_uri_complete", "")),
    )
    interval = float(start.get("interval", 5))
    deadline = time.monotonic() + float(start.get("expires_in", 600))
    while time.monotonic() < deadline:
        sleep(interval)
        body = http.post(
            cfg.token_endpoint,
            data={
                "grant_type": "urn:ietf:params:oauth:grant-type:device_code",
                "device_code": start["device_code"],
                "client_id": cfg.client_id,
            },
        ).json()
        error = body.get("error")
        if error == "authorization_pending":
            continue
        if error == "slow_down":
            interval += 5
            continue
        return _token_response(cfg, body)
    raise AuthError("the device code expired before it was approved")


# --- authorization code with PKCE and a loopback redirect (RFC 7636, RFC 8252) ---


def pkce_pair() -> tuple[str, str]:
    verifier = secrets.token_urlsafe(64)
    digest = hashlib.sha256(verifier.encode()).digest()
    return verifier, base64.urlsafe_b64encode(digest).rstrip(b"=").decode()


def browser_login(
    cfg: LoginConfig,
    http: httpx.Client,
    *,
    open_url: Callable[[str], Any] = webbrowser.open,
    timeout_s: float = 300.0,
) -> Credentials:
    """Open the provider's sign-in page and catch the code on a loopback port."""
    if not cfg.authorization_endpoint:
        raise AuthError("the identity provider has no authorization endpoint")
    verifier, challenge = pkce_pair()
    state = secrets.token_urlsafe(24)
    result: dict[str, str] = {}
    done = threading.Event()

    class Handler(http_server.BaseHTTPRequestHandler):
        def do_GET(self) -> None:
            query = dict(urllib.parse.parse_qsl(urllib.parse.urlsplit(self.path).query))
            ok = query.get("state") == state and "code" in query
            if ok:
                result.update(query)
            elif "error" in query:
                result["error"] = query.get("error_description") or query["error"]
            self.send_response(200 if ok else 400)
            self.send_header("Content-Type", "text/plain; charset=utf-8")
            self.end_headers()
            message = (
                "Signed in to Evals.si. You can close this window." if ok else "Sign-in failed."
            )
            self.wfile.write(message.encode())
            if ok or "error" in result:
                done.set()

        def log_message(self, *_: Any) -> None:
            pass

    server = http_server.HTTPServer(("127.0.0.1", 0), Handler)
    redirect = f"http://127.0.0.1:{server.server_address[1]}/callback"
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        params = {
            "response_type": "code",
            "client_id": cfg.client_id,
            "redirect_uri": redirect,
            "scope": " ".join(cfg.scopes),
            "state": state,
            "code_challenge": challenge,
            "code_challenge_method": "S256",
        }
        if cfg.audience:
            params["audience"] = cfg.audience
        url = cfg.authorization_endpoint + "?" + urllib.parse.urlencode(params)
        if not open_url(url):
            print(f"Open this URL to sign in:\n  {url}")
        if not done.wait(timeout_s):
            raise AuthError("timed out waiting for the browser sign-in")
    finally:
        server.shutdown()
        server.server_close()
    if "error" in result:
        raise AuthError(f"sign-in failed: {result['error']}")
    body = http.post(
        cfg.token_endpoint,
        data={
            "grant_type": "authorization_code",
            "code": result["code"],
            "redirect_uri": redirect,
            "client_id": cfg.client_id,
            "code_verifier": verifier,
        },
    ).json()
    return _token_response(cfg, body)


def refresh(creds: Credentials, http: httpx.Client) -> Credentials:
    """Use the refresh token; raises AuthError when the login must be redone."""
    if not creds.refresh_token or not creds.token_endpoint:
        raise AuthError("the login expired; run `evalsi login` again")
    body = http.post(
        creds.token_endpoint,
        data={
            "grant_type": "refresh_token",
            "refresh_token": creds.refresh_token,
            "client_id": creds.client_id,
        },
    ).json()
    if "error" in body:
        raise AuthError(f"refreshing the login failed ({body['error']}); run `evalsi login` again")
    cfg = LoginConfig(creds.issuer, creds.client_id, creds.scopes, "", creds.token_endpoint, "", "")
    fresh = _token_response(cfg, body)
    # Providers may not rotate refresh tokens or reissue ID tokens.
    fresh.refresh_token = fresh.refresh_token or creds.refresh_token
    fresh.id_token = fresh.id_token or (
        "" if fresh.access_token.count(".") == 2 else creds.id_token
    )
    return fresh


# --- GitHub Actions OIDC ---


def github_actions_token(audience: str, http: httpx.Client) -> str:
    """The job's OIDC token for an audience (needs `permissions: id-token: write`)."""
    url = os.environ.get("ACTIONS_ID_TOKEN_REQUEST_URL")
    request_token = os.environ.get("ACTIONS_ID_TOKEN_REQUEST_TOKEN")
    if not url or not request_token:
        raise AuthError(
            "EVALSI_OIDC_AUDIENCE is set but this is not a GitHub Actions job with "
            "`permissions: id-token: write`"
        )
    resp = http.get(
        url,
        params={"audience": audience},
        headers={"Authorization": f"Bearer {request_token}"},
    )
    if resp.status_code != 200:
        raise AuthError(f"fetching the GitHub Actions OIDC token: HTTP {resp.status_code}")
    return str(resp.json()["value"])


# --- resolution, and the httpx auth flow the client uses ---


class ServerAuth(httpx.Auth):
    """Adds the server's credential to every request, refreshing a cached
    login when it is about to expire."""

    def __init__(
        self,
        server: str,
        *,
        token: str | None = None,
        api_key: str | None = None,
        http: httpx.Client | None = None,
    ) -> None:
        self.server = server
        self._http = http
        self._lock = threading.Lock()
        self._fixed: dict[str, str] = {}
        self._github_audience = ""
        self._github_token = ""
        token = token or None
        api_key = api_key or None
        if api_key is None and token is None:
            api_key = os.environ.get("EVALSI_API_KEY") or None
            token = os.environ.get("EVALSI_TOKEN") or None
        if api_key:
            self._fixed = {"Authorization": f"Bearer {api_key}"}
        elif token:
            self._fixed = {"Authorization": f"Bearer {token}"}
        elif audience := os.environ.get("EVALSI_OIDC_AUDIENCE"):
            self._github_audience = audience

    @property
    def kind(self) -> str:
        """What credential is in use: api-key, token, github-actions, login or none."""
        if self._fixed:
            value = self._fixed["Authorization"].removeprefix("Bearer ")
            return "api-key" if value.startswith(API_KEY_PREFIX) else "token"
        if self._github_audience:
            return "github-actions"
        return "login" if load_credentials(self.server) else "none"

    def _client(self) -> httpx.Client:
        if self._http is None:
            self._http = httpx.Client(timeout=30.0)
        return self._http

    def headers(self) -> dict[str, str]:
        if self._fixed:
            return dict(self._fixed)
        with self._lock:
            if self._github_audience:
                if not self._github_token or Credentials(id_token=self._github_token).expired():
                    self._github_token = github_actions_token(self._github_audience, self._client())
                return {"Authorization": f"Bearer {self._github_token}"}
            creds = load_credentials(self.server)
            if creds is None:
                return {}
            if creds.expired():
                creds = refresh(creds, self._client())
                save_credentials(self.server, creds)
            return {"Authorization": f"Bearer {creds.bearer()}"}

    def auth_flow(self, request: httpx.Request) -> Generator[httpx.Request, httpx.Response, None]:
        request.headers.update(self.headers())
        yield request
