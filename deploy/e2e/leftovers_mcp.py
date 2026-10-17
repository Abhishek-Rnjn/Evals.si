"""Interoperate with mcp 1.26.0 FastMCP and Authlib 1.6.5 client credentials.

All services bind loopback and use ephemeral test credentials. No provider or
model access is needed. Elicitation is sent by the published SDK server.
"""

from __future__ import annotations

import asyncio
import json
import os
import secrets
import socket
import threading
import time
from pathlib import Path
from typing import Any

import uvicorn
from authlib.integrations.flask_oauth2 import AuthorizationServer
from authlib.oauth2.rfc6749 import ClientMixin, grants
from flask import Flask
from mcp.server.auth.provider import AccessToken, TokenVerifier
from mcp.server.auth.settings import AuthSettings
from mcp.server.fastmcp import Context, FastMCP
from pydantic import BaseModel
from werkzeug.serving import make_server

from evalsi_harness.mcp import MCPClient, MCPError, OAuth


def port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return int(s.getsockname()[1])


def start_authority(
    oauth_port: int, secret: str, tokens: dict[str, dict[str, Any]]
) -> tuple[Any, threading.Thread]:
    class Client(ClientMixin):
        def get_client_id(self) -> str:
            return "evalsi-check"

        def check_client_secret(self, value: str) -> bool:
            return secrets.compare_digest(value, secret)

        def check_endpoint_auth_method(self, method: str, endpoint: str) -> bool:
            return method == "client_secret_basic"

        def check_grant_type(self, grant_type: str) -> bool:
            return grant_type == "client_credentials"

        def get_allowed_scope(self, scope: str) -> str:
            return "verify" if scope == "verify" else ""

    app = Flask(__name__)
    app.config.update(
        OAUTH2_ACCESS_TOKEN_GENERATOR=True, OAUTH2_TOKEN_EXPIRES_IN={"client_credentials": 3600}
    )

    def save(token: dict[str, Any], request: Any) -> None:
        tokens[token["access_token"]] = dict(token)

    authority = AuthorizationServer(
        app,
        query_client=lambda client_id: Client() if client_id == "evalsi-check" else None,
        save_token=save,
    )
    authority.register_grant(grants.ClientCredentialsGrant)
    app.add_url_rule("/token", "token", authority.create_token_response, methods=["POST"])
    http = make_server("127.0.0.1", oauth_port, app)
    http_thread = threading.Thread(target=http.serve_forever, daemon=True)
    http_thread.start()

    return http, http_thread


async def exercise_client(
    mcp: FastMCP,
    resource: str,
    issuer: str,
    secret: str,
    *,
    tokens: dict[str, dict[str, Any]],
    revoked: set[str],
) -> dict[str, Any]:
    asked: list[str] = []

    async def answer(message: str, schema: dict[str, Any]) -> dict[str, Any]:
        asked.append(message)
        assert "approved" in schema["properties"]
        return {"action": "accept", "content": {"approved": True}}

    for credential in [None, OAuth(issuer + "/token", "evalsi-check", "wrong", "verify")]:
        try:
            denied = await MCPClient.connect(url=resource, oauth=credential)
        except MCPError:
            pass
        else:
            await denied.aclose()
            raise AssertionError("unauthorized SDK connection accepted")
    oauth = OAuth(issuer + "/token", "evalsi-check", secret, "verify")
    client = await MCPClient.connect(url=resource, oauth=oauth, elicit=answer)
    try:
        assert {t.name for t in await client.list_tools()} == {"ask", "echo"}
        assert (await client.call_tool("echo", {"text": "actual SDK"})).content == "actual SDK"
        result = await client.call_tool("ask", {})
        assert not result.is_error, result
        assert result.content == "accept:True", result
        assert len(asked) == 1
        assert (await client.call("resources/read", {"uri": "check://readme"}))["contents"][0][
            "text"
        ] == "SDK resource"
        assert len(tokens) == 1, len(tokens)
        revoked.update(tokens)
        result = await client.call_tool("echo", {"text": "refreshed"})
        assert result.content == "refreshed", result
        assert len(tokens) == 2, len(tokens)
    finally:
        await client.aclose()
    for _ in range(100):
        if all(s.is_terminated for s in mcp.session_manager._server_instances.values()):
            break
        await asyncio.sleep(0.01)
    assert all(s.is_terminated for s in mcp.session_manager._server_instances.values()), (
        "authenticated DELETE leaked its session"
    )
    report = {
        "mcp": "1.26.0",
        "authlib": "1.6.5",
        "transport": "streamable HTTP/SSE",
        "passed": [
            "anonymous rejected",
            "wrong client secret rejected",
            "tools and resources",
            "server-initiated elicitation",
            "token cache",
            "401 token refresh",
            "authenticated session termination",
        ],
    }
    print(json.dumps(report))
    return report


async def verify() -> dict[str, Any]:
    oauth_port, mcp_port = port(), port()
    issuer = f"http://127.0.0.1:{oauth_port}"
    resource = f"http://127.0.0.1:{mcp_port}/mcp"
    secret = secrets.token_urlsafe(24)
    tokens: dict[str, dict[str, Any]] = {}
    revoked: set[str] = set()
    # Authlib intentionally permits insecure transport only for loopback testing.
    os.environ["AUTHLIB_INSECURE_TRANSPORT"] = "1"

    http, http_thread = start_authority(oauth_port, secret, tokens)

    class Verifier(TokenVerifier):
        async def verify_token(self, token: str) -> AccessToken | None:
            if token not in tokens or token in revoked:
                return None
            return AccessToken(
                token=token,
                client_id="evalsi-check",
                scopes=["verify"],
                expires_at=int(time.time()) + 3600,
            )

    mcp = FastMCP(
        "evalsi-live-interoperability",
        host="127.0.0.1",
        port=mcp_port,
        token_verifier=Verifier(),
        auth=AuthSettings(
            issuer_url=issuer, resource_server_url=resource, required_scopes=["verify"]
        ),
    )

    class Answer(BaseModel):
        approved: bool

    @mcp.tool()
    async def ask(ctx: Context) -> str:
        result = await ctx.elicit("Approve this local check?", Answer)
        return f"{result.action}:{result.data.approved if result.data else False}"

    @mcp.tool()
    def echo(text: str) -> str:
        return text

    @mcp.resource("check://readme")
    def readme() -> str:
        return "SDK resource"

    server = uvicorn.Server(
        uvicorn.Config(
            mcp.streamable_http_app(), host="127.0.0.1", port=mcp_port, log_level="error"
        )
    )
    thread = threading.Thread(target=server.run, daemon=True)
    thread.start()
    for _ in range(100):
        if server.started:
            break
        await asyncio.sleep(0.05)
    assert server.started
    try:
        return await exercise_client(mcp, resource, issuer, secret, tokens=tokens, revoked=revoked)
    finally:
        server.should_exit = True
        await asyncio.to_thread(thread.join, 10)
        http.shutdown()
        http.server_close()
        http_thread.join(5)


if __name__ == "__main__":
    import argparse

    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--report", type=Path, required=True)
    args = parser.parse_args()
    args.report.write_text(json.dumps(asyncio.run(verify()), indent=2) + "\n")
