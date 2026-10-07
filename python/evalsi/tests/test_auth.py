from __future__ import annotations

import argparse
import base64
import json
import stat
import time
import urllib.parse
from pathlib import Path
from typing import Any

import httpx
import pytest

from evalsi import auth, cli, cli_auth
from evalsi.client import Client, ServerError

SERVER = "http://evals.test"
ISSUER = "https://sso.test"


def fake_jwt(exp: float) -> str:
    def enc(obj: dict[str, Any]) -> str:
        return base64.urlsafe_b64encode(json.dumps(obj).encode()).rstrip(b"=").decode()

    return f"{enc({'alg': 'RS256'})}.{enc({'sub': 'alice', 'exp': exp})}.sig"


@pytest.fixture(autouse=True)
def clean_env(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> Path:
    for name in (
        "EVALSI_API_KEY",
        "EVALSI_TOKEN",
        "EVALSI_TOKEN_FILE",
        "EVALSI_OIDC_AUDIENCE",
        "ACTIONS_ID_TOKEN_REQUEST_URL",
        "ACTIONS_ID_TOKEN_REQUEST_TOKEN",
        "EVALSI_SERVER",
    ):
        monkeypatch.delenv(name, raising=False)
    path = tmp_path / "cfg" / "credentials"
    monkeypatch.setenv("EVALSI_CREDENTIALS", str(path))
    return path


class IdP:
    """An identity provider and evalsid stand-in on one mock transport."""

    def __init__(self) -> None:
        self.polls = 0
        self.refreshes = 0
        self.seen_auth: list[str] = []
        self.codes: dict[str, str] = {}

    def handler(self, request: httpx.Request) -> httpx.Response:
        url = str(request.url)
        form = dict(urllib.parse.parse_qsl(request.content.decode())) if request.content else {}
        if url == SERVER + "/.well-known/evalsi-auth":
            return httpx.Response(
                200, json={"auth_enabled": True, "issuer": ISSUER, "client_id": "evalsi-cli"}
            )
        if url == ISSUER + "/.well-known/openid-configuration":
            return httpx.Response(
                200,
                json={
                    "issuer": ISSUER,
                    "token_endpoint": ISSUER + "/token",
                    "device_authorization_endpoint": ISSUER + "/device",
                    "authorization_endpoint": ISSUER + "/authorize",
                },
            )
        if url == ISSUER + "/device":
            assert form["client_id"] == "evalsi-cli"
            return httpx.Response(
                200,
                json={
                    "device_code": "dc",
                    "user_code": "ABCD-EFGH",
                    "verification_uri": ISSUER + "/activate",
                    "interval": 1,
                    "expires_in": 60,
                },
            )
        if url == ISSUER + "/token":
            grant = form["grant_type"]
            if grant.endswith("device_code"):
                self.polls += 1
                if self.polls == 1:
                    return httpx.Response(400, json={"error": "authorization_pending"})
                if self.polls == 2:
                    return httpx.Response(400, json={"error": "slow_down"})
            if grant == "refresh_token":
                self.refreshes += 1
                assert form["refresh_token"] == "rt"
                return httpx.Response(
                    200, json={"id_token": fake_jwt(time.time() + 3600), "expires_in": 3600}
                )
            if grant == "authorization_code":
                verifier = form["code_verifier"]
                challenge = self.codes[form["code"]]
                digest = base64.urlsafe_b64encode(
                    __import__("hashlib").sha256(verifier.encode()).digest()
                )
                if digest.rstrip(b"=").decode() != challenge:
                    return httpx.Response(400, json={"error": "invalid_grant"})
            return httpx.Response(
                200,
                json={
                    "access_token": "opaque",
                    "id_token": fake_jwt(time.time() + 3600),
                    "refresh_token": "rt",
                    "expires_in": 3600,
                },
            )
        if url == "https://gh.test/token?audience=https%3A%2F%2Fevals.test":
            assert request.headers["Authorization"] == "Bearer gh-request"
            return httpx.Response(200, json={"value": fake_jwt(time.time() + 300)})
        if url.startswith(SERVER + "/evalsi.v1alpha1.AuthService/"):
            self.seen_auth.append(request.headers.get("Authorization", ""))
            method = url.rsplit("/", 1)[1]
            if not request.headers.get("Authorization"):
                return httpx.Response(401, json={"code": "unauthenticated", "message": "no"})
            body = json.loads(request.content or b"{}")
            if method == "WhoAmI":
                return httpx.Response(
                    200,
                    json={
                        "principal": {"id": "user:corp/alice", "kind": "user", "method": "jwt"},
                        "authEnabled": True,
                        "access": [
                            {
                                "project": "support",
                                "roles": ["runner"],
                                "permissions": ["runs.create"],
                            }
                        ],
                    },
                )
            if method == "CreateAPIKey":
                return httpx.Response(
                    200, json={"key": {"name": body["name"]}, "secret": "evk_new", "body": body}
                )
            if method == "ListAPIKeys":
                return httpx.Response(
                    200,
                    json={
                        "keys": [
                            {
                                "name": "ci",
                                "roles": {"support": {"roles": ["runner"]}},
                                "source": "api",
                            }
                        ]
                    },
                )
            return httpx.Response(200, json={"echo": body, "method": method})
        return httpx.Response(404, json={"url": url})

    def client(self) -> httpx.Client:
        return httpx.Client(transport=httpx.MockTransport(self.handler))


def test_credentials_cache_is_private(clean_env: Path) -> None:
    creds = auth.Credentials(id_token="t", refresh_token="r", token_endpoint="e")
    auth.save_credentials(SERVER + "/", creds)
    assert stat.S_IMODE(clean_env.stat().st_mode) == 0o600
    loaded = auth.load_credentials(SERVER)
    assert loaded is not None
    assert loaded.refresh_token == "r"
    assert auth.delete_credentials(SERVER)
    assert auth.load_credentials(SERVER) is None
    assert not auth.delete_credentials(SERVER)


def test_token_expiry() -> None:
    assert auth.jwt_expiry(fake_jwt(123)) == 123
    assert auth.jwt_expiry("opaque") is None
    assert auth.Credentials(id_token=fake_jwt(time.time() + 30)).expired()
    assert not auth.Credentials(id_token=fake_jwt(time.time() + 3600)).expired()
    # A JWT access token is preferred: it is the audience-bound API token.
    jwt_access = fake_jwt(1)
    assert auth.Credentials(access_token=jwt_access, id_token="x").bearer() == jwt_access
    assert auth.Credentials(access_token="opaque", id_token="idt").bearer() == "idt"


def test_resolution_order(monkeypatch: pytest.MonkeyPatch) -> None:
    def header(**kw: Any) -> str:
        return auth.ServerAuth(SERVER, **kw).headers().get("Authorization", "")

    assert header() == ""
    auth.save_credentials(SERVER, auth.Credentials(id_token=fake_jwt(time.time() + 3600)))
    assert header().startswith("Bearer ey")
    monkeypatch.setenv("EVALSI_TOKEN", "env-token")
    assert header() == "Bearer env-token"
    monkeypatch.setenv("EVALSI_API_KEY", "evk_env")
    assert header() == "Bearer evk_env"
    assert header(token="flag-token") == "Bearer flag-token"
    assert auth.ServerAuth(SERVER, api_key="evk_x").kind == "api-key"


def test_token_file_is_read_per_request(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    # A projected service-account token, which the kubelet rotates in place.
    path = tmp_path / "token"
    path.write_text("sa-1\n")
    monkeypatch.setenv("EVALSI_TOKEN_FILE", str(path))
    a = auth.ServerAuth(SERVER)
    assert (a.kind, a.headers()) == ("token-file", {"Authorization": "Bearer sa-1"})
    path.write_text("sa-2\n")
    assert a.headers() == {"Authorization": "Bearer sa-2"}
    monkeypatch.setenv("EVALSI_TOKEN", "env-token")
    assert auth.ServerAuth(SERVER).headers() == {"Authorization": "Bearer env-token"}


def test_github_actions_oidc(monkeypatch: pytest.MonkeyPatch) -> None:
    idp = IdP()
    monkeypatch.setenv("EVALSI_OIDC_AUDIENCE", "https://evals.test")
    with pytest.raises(auth.AuthError, match="id-token: write"):
        auth.ServerAuth(SERVER, http=idp.client()).headers()
    monkeypatch.setenv("ACTIONS_ID_TOKEN_REQUEST_URL", "https://gh.test/token")
    monkeypatch.setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "gh-request")
    a = auth.ServerAuth(SERVER, http=idp.client())
    assert a.kind == "github-actions"
    assert a.headers()["Authorization"].startswith("Bearer ey")


def test_device_login_and_refresh() -> None:
    idp = IdP()
    http = idp.client()
    cfg = auth.discover(SERVER, http)
    shown: list[str] = []
    sleeps: list[float] = []
    creds = auth.device_login(
        cfg, http, show=lambda uri, code, _: shown.append(code), sleep=sleeps.append
    )
    assert shown == ["ABCD-EFGH"]
    assert sleeps == [1, 1, 6]  # slow_down adds five seconds
    assert creds.refresh_token == "rt"
    assert creds.bearer().startswith("ey")

    creds.expires_at = time.time() - 1
    auth.save_credentials(SERVER, creds)
    a = auth.ServerAuth(SERVER, http=http)
    assert a.headers()["Authorization"].startswith("Bearer ey")
    assert idp.refreshes == 1
    stored = auth.load_credentials(SERVER)
    assert stored is not None
    assert stored.refresh_token == "rt"
    assert not stored.expired()


def test_refresh_without_refresh_token_asks_to_log_in() -> None:
    with pytest.raises(auth.AuthError, match="evalsi login"):
        auth.refresh(auth.Credentials(), httpx.Client())


def test_browser_login_with_pkce() -> None:
    idp = IdP()
    http = idp.client()
    cfg = auth.discover(SERVER, http)

    def browser(url: str) -> bool:
        query = dict(urllib.parse.parse_qsl(urllib.parse.urlsplit(url).query))
        assert query["code_challenge_method"] == "S256"
        idp.codes["the-code"] = query["code_challenge"]
        redirect = (
            query["redirect_uri"]
            + "?"
            + urllib.parse.urlencode({"code": "the-code", "state": query["state"]})
        )
        # A forged callback with the wrong state is ignored.
        assert httpx.get(query["redirect_uri"] + "?code=x&state=nope").status_code == 400
        assert httpx.get(redirect).status_code == 200
        return True

    creds = auth.browser_login(cfg, http, open_url=browser, timeout_s=10)
    assert creds.refresh_token == "rt"


def test_client_sends_credentials_and_hints() -> None:
    idp = IdP()
    transport = httpx.MockTransport(idp.handler)
    with Client(SERVER, transport=transport, api_key="evk_k") as client:
        assert client.whoami()["principal"]["id"] == "user:corp/alice"
    assert idp.seen_auth[-1] == "Bearer evk_k"
    with Client(SERVER, transport=transport) as client, pytest.raises(ServerError) as err:
        client.whoami()
    assert "evalsi login" in str(err.value)


def test_parsers() -> None:
    assert cli_auth.parse_ttl("30d") == "2592000s"
    assert cli_auth.parse_ttl("90m") == "5400s"
    with pytest.raises(ValueError, match="--ttl"):
        cli_auth.parse_ttl("soon")
    assert cli_auth.parse_roles(["support=runner,viewer", "*=ingest"]) == {
        "support": {"roles": ["runner", "viewer"]},
        "*": {"roles": ["ingest"]},
    }
    with pytest.raises(ValueError, match="PROJECT=ROLE"):
        cli_auth.parse_roles(["runner"])
    assert cli_auth.parse_labels(["app=checkout"]) == {"app": "checkout"}


@pytest.fixture
def mock_server(monkeypatch: pytest.MonkeyPatch) -> IdP:
    idp = IdP()

    def factory(args: argparse.Namespace) -> Client:
        return Client(
            args.server,
            transport=httpx.MockTransport(idp.handler),
            token=getattr(args, "token", None),
            api_key=getattr(args, "api_key", None),
        )

    monkeypatch.setattr(cli, "server_client", factory)
    return idp


def test_cli_whoami_and_keys(mock_server: IdP, capsys: pytest.CaptureFixture[str]) -> None:
    assert cli.main(["whoami", "--server", SERVER, "--api-key", "evk_k"]) == 0
    out = capsys.readouterr().out
    for want in ("user:corp/alice", "support", "api-key"):
        assert want in out

    args = ["auth", "keys", "create", "ci", "--role", "support=runner", "--ttl", "1d"]
    assert cli.main([*args, "--server", SERVER, "--token", "t", "--label", "team=x"]) == 0
    out = capsys.readouterr().out
    assert "evk_new" in out
    assert "not shown again" in out

    assert cli.main(["auth", "keys", "list", "--server", SERVER, "--token", "t"]) == 0
    assert "support=runner" in capsys.readouterr().out

    assert (
        cli.main(
            [
                "auth",
                "roles",
                "create",
                "prompt-engineer",
                "--project",
                "support",
                "--inherits",
                "viewer",
                "--permission",
                "runs.create",
                "--condition",
                "resource.target.model == 'qwen3'",
                "--server",
                SERVER,
                "--token",
                "t",
                "--format",
                "json",
            ]
        )
        == 0
    )
    echoed = json.loads(capsys.readouterr().out)
    assert echoed["method"] == "CreateRole"
    assert echoed["echo"]["role"]["inherits"] == ["viewer"]


def test_cli_reports_unauthenticated(mock_server: IdP, capsys: pytest.CaptureFixture[str]) -> None:
    assert cli.main(["whoami", "--server", SERVER]) == 1
    assert "evalsi login" in capsys.readouterr().err


def test_cli_logout(capsys: pytest.CaptureFixture[str]) -> None:
    auth.save_credentials(SERVER, auth.Credentials(id_token="x"))
    assert cli.main(["logout", "--server", SERVER]) == 0
    assert "signed out" in capsys.readouterr().out
    assert auth.load_credentials(SERVER) is None


def test_auth_token_prints_the_cached_login(capsys: pytest.CaptureFixture[str]) -> None:
    assert cli.main(["auth", "token", "--server", SERVER]) == 1
    assert "no login for" in capsys.readouterr().err
    token = fake_jwt(time.time() + 3600)
    auth.save_credentials(SERVER, auth.Credentials(id_token=token))
    assert cli.main(["auth", "token", "--server", SERVER]) == 0
    assert capsys.readouterr().out.strip() == token
