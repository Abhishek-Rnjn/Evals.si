"""``evalsi login``, ``logout``, ``whoami`` and ``evalsi auth ...``."""

from __future__ import annotations

import argparse
import json
import os
import re
import sys
from typing import Any

import httpx

from evalsi import auth


def _server_arg(parser: argparse.ArgumentParser) -> None:
    default = os.environ.get("EVALSI_SERVER")
    parser.add_argument(
        "--server",
        default=default,
        required=default is None,
        help="evalsid URL (default: EVALSI_SERVER)",
    )


def add_auth_commands(sub: Any) -> None:
    from evalsi.cli import add_credential_args

    login = sub.add_parser("login", help="sign in to a server with your identity provider")
    _server_arg(login)
    how = login.add_mutually_exclusive_group()
    how.add_argument(
        "--browser", action="store_true", help="sign in in a browser (PKCE, loopback redirect)"
    )
    how.add_argument("--device", action="store_true", help="sign in with a device code (default)")
    login.set_defaults(func=_cmd_login)

    logout = sub.add_parser("logout", help="forget the cached login for a server")
    _server_arg(logout)
    logout.set_defaults(func=_cmd_logout)

    who = sub.add_parser("whoami", help="show who the server thinks you are, and your roles")
    _server_arg(who)
    who.add_argument("--format", choices=["table", "json"], default="table")
    add_credential_args(who)
    who.set_defaults(func=_cmd_whoami)

    au = sub.add_parser(
        "auth", help="manage projects, API keys, roles and bindings; read the audit"
    )
    au_sub = au.add_subparsers(dest="area", required=True)
    parsers = _add_keys(au_sub) + _add_roles(au_sub) + _add_bindings(au_sub)
    parsers += _add_projects_and_audit(au_sub)
    for p in parsers:
        _server_arg(p)
        p.add_argument("--format", choices=["table", "json"], default="table")
        add_credential_args(p)
    tok = au_sub.add_parser(
        "token", help="print your login's token for a server (for the web UI or curl)"
    )
    _server_arg(tok)
    au.set_defaults(func=_cmd_auth)


def _add_keys(au_sub: Any) -> list[argparse.ArgumentParser]:
    keys = au_sub.add_parser("keys", help="API keys").add_subparsers(dest="action", required=True)
    kc = keys.add_parser("create", help="create a key; the key is shown once")
    kc.add_argument("name")
    kc.add_argument(
        "--role",
        action="append",
        required=True,
        metavar="PROJECT=ROLE[,ROLE]",
        help="roles in a project ('*' for every project); repeatable",
    )
    kc.add_argument("--label", action="append", default=[], metavar="KEY=VALUE")
    kc.add_argument("--ttl", help="lifetime, for example 720h or 30d; no expiry by default")
    kl = keys.add_parser("list", help="list keys you manage")
    kl.add_argument("--project", default="")
    kr = keys.add_parser("revoke", help="revoke a key")
    kr.add_argument("name")
    return [kc, kl, kr]


def _add_roles(au_sub: Any) -> list[argparse.ArgumentParser]:
    roles = au_sub.add_parser("roles", help="custom roles").add_subparsers(
        dest="action", required=True
    )
    rl = roles.add_parser("list", help="list roles")
    rl.add_argument("--project", default="")
    rc = roles.add_parser("create", help="create a custom role")
    ru = roles.add_parser("update", help="replace a custom role's definition")
    for p in (rc, ru):
        p.add_argument("name", nargs="?", help="role name (or give -f)")
        p.add_argument("-f", "--file", help="YAML or JSON role definition")
        p.add_argument("--project", default="", help="the project; install-wide when omitted")
        p.add_argument("--permission", action="append", default=[], dest="permissions")
        p.add_argument("--inherits", action="append", default=[])
        p.add_argument("--condition", default="", help="CEL expression that must hold")
        p.add_argument("--description", default="")
    rd = roles.add_parser("delete", help="delete a custom role")
    rd.add_argument("name")
    rd.add_argument("--project", default="")
    rp = roles.add_parser("permissions", help="list the permissions roles can grant")
    return [rl, rc, ru, rd, rp]


def _add_bindings(au_sub: Any) -> list[argparse.ArgumentParser]:
    bindings = au_sub.add_parser("bindings", help="role bindings").add_subparsers(
        dest="action", required=True
    )
    bl = bindings.add_parser("list", help="list bindings you manage")
    bl.add_argument("--project", default="")
    bc = bindings.add_parser("create", help="grant a role to a subject in a project")
    bdel = bindings.add_parser("delete", help="remove a binding")
    for p in (bc, bdel):
        p.add_argument("--project", required=True, help="a project, or '*' for every project")
        p.add_argument("--role", required=True)
        p.add_argument(
            "--subject",
            required=True,
            help="user:<provider>/<sub>, email:<addr>, group:<provider>/<group>, key:<name> "
            "or cel:<expression>",
        )
    return [bl, bc, bdel]


def _add_projects_and_audit(au_sub: Any) -> list[argparse.ArgumentParser]:
    projects = au_sub.add_parser("projects", help="projects").add_subparsers(
        dest="action", required=True
    )
    pl = projects.add_parser("list", help="list projects you can see")
    pc = projects.add_parser("create", help="create a project (owners only)")
    pc.add_argument("name")
    pc.add_argument("--description", default="")

    audit = au_sub.add_parser("audit", help="read the audit log")
    audit.add_argument("--project", default="")
    audit.add_argument("--action", dest="audit_action", default="")
    audit.add_argument("--denied", action="store_true", help="only denied calls")
    audit.add_argument("--limit", type=int, default=50)
    return [pl, pc, audit]


# --- login ---


def _show_device_code(uri: str, code: str, complete: str) -> None:
    print(f"To sign in, open {complete or uri}", file=sys.stderr)
    if code:
        print(f"and enter the code: {code}", file=sys.stderr)
    print("Waiting for approval...", file=sys.stderr)


def _cmd_login(args: argparse.Namespace) -> int:
    with httpx.Client(timeout=30.0) as http:
        cfg = auth.discover(args.server, http)
        if args.browser or (not args.device and not cfg.device_endpoint):
            creds = auth.browser_login(cfg, http)
        else:
            creds = auth.device_login(cfg, http, show=_show_device_code)
    auth.save_credentials(args.server, creds)
    from evalsi.cli import server_client

    with server_client(argparse.Namespace(server=args.server)) as client:
        me = client.whoami()
    principal = me.get("principal", {})
    print(f"signed in to {args.server} as {principal.get('id', '?')}")
    return 0


def _cmd_logout(args: argparse.Namespace) -> int:
    if auth.delete_credentials(args.server):
        print(f"signed out of {args.server}")
    else:
        print(f"no cached login for {args.server}")
    return 0


def _cmd_whoami(args: argparse.Namespace) -> int:
    from evalsi.cli import server_client

    with server_client(args) as client:
        me = client.whoami()
        source = client.auth.kind
    if args.format == "json":
        print(json.dumps(me, indent=2))
        return 0
    p = me.get("principal", {})
    if not me.get("authEnabled"):
        print("authentication is disabled on this server")
    print(f"principal: {p.get('id', 'anonymous')} ({p.get('kind', '')} via {p.get('method', '')})")
    if p.get("name") or p.get("email"):
        print(f"name:      {p.get('name', '')} {p.get('email', '')}".rstrip())
    if p.get("groups"):
        print(f"groups:    {', '.join(p['groups'])}")
    print(f"credential: {source}")
    if me.get("owner"):
        print("owner:     yes (every permission in every project)")
    rows = [["project", "roles", "permissions"]]
    for a in me.get("access", []):
        rows.append(
            [a["project"], ",".join(a.get("roles", [])), ",".join(a.get("permissions", []))]
        )
    if len(rows) > 1:
        print()
        _table(rows)
    return 0


# --- auth ---

_DURATION = re.compile(r"^(\d+)([smhd])$")


def parse_ttl(text: str) -> str:
    """'30d' -> '2592000s' (protobuf Duration JSON)."""
    m = _DURATION.match(text.strip())
    if not m:
        raise ValueError(f"--ttl {text!r}: use a number and s, m, h or d, for example 720h")
    n = int(m.group(1)) * {"s": 1, "m": 60, "h": 3600, "d": 86400}[m.group(2)]
    return f"{n}s"


def parse_roles(specs: list[str]) -> dict[str, dict[str, list[str]]]:
    out: dict[str, dict[str, list[str]]] = {}
    for spec in specs:
        project, sep, roles = spec.partition("=")
        if not sep or not project or not roles:
            raise ValueError(f"--role {spec!r}: use PROJECT=ROLE[,ROLE]")
        out.setdefault(project, {"roles": []})["roles"] += [r for r in roles.split(",") if r]
    return out


def parse_labels(specs: list[str]) -> dict[str, str]:
    out = {}
    for spec in specs:
        key, sep, value = spec.partition("=")
        if not sep or not key:
            raise ValueError(f"--label {spec!r}: use KEY=VALUE")
        out[key] = value
    return out


def _role_body(args: argparse.Namespace) -> dict[str, Any]:
    role: dict[str, Any] = {}
    if args.file:
        import yaml

        with open(args.file, encoding="utf-8") as handle:
            loaded = yaml.safe_load(handle)
        if not isinstance(loaded, dict):
            raise ValueError(f"{args.file}: expected a mapping")
        role.update(loaded)
    for key, value in (
        ("name", args.name),
        ("project", args.project),
        ("permissions", args.permissions),
        ("inherits", args.inherits),
        ("condition", args.condition),
        ("description", args.description),
    ):
        if value:
            role[key] = value
    if not role.get("name"):
        raise ValueError("give the role a name, or a file with one")
    return role


def _table(rows: list[list[str]]) -> None:
    widths = [max(len(r[i]) for r in rows) for i in range(len(rows[0]))]
    for row in rows:
        print("  ".join(cell.ljust(w) for cell, w in zip(row, widths, strict=True)).rstrip())


def _cmd_token(args: argparse.Namespace) -> int:
    import httpx

    from evalsi.auth import AuthError, load_credentials, refresh, save_credentials

    creds = load_credentials(args.server)
    if creds is None:
        raise AuthError(f"no login for {args.server}; run `evalsi login --server {args.server}`")
    if creds.expired():
        with httpx.Client(timeout=30) as http:
            creds = refresh(creds, http)
        save_credentials(args.server, creds)
    print(creds.bearer())
    return 0


def _cmd_auth(args: argparse.Namespace) -> int:
    from evalsi.cli import server_client

    if args.area == "token":
        return _cmd_token(args)

    with server_client(args) as client:
        out, rows = _auth_action(client, args)
    if args.format == "json":
        print(json.dumps(out, indent=2))
    elif isinstance(rows, str):
        print(rows)
    elif rows:
        _table(rows)
    return 0


Rows = list[list[str]] | str


def _auth_action(client: Any, args: argparse.Namespace) -> tuple[Any, Rows]:
    """Calls AuthService; returns the response and either table rows or a message."""
    area, action = args.area, getattr(args, "action", "")
    call = client.auth_call
    if area == "keys":
        if action == "create":
            body: dict[str, Any] = {
                "name": args.name,
                "roles": parse_roles(args.role),
                "labels": parse_labels(args.label),
            }
            if args.ttl:
                body["ttl"] = parse_ttl(args.ttl)
            out = call("CreateAPIKey", body)
            note = f"created API key {args.name}. Store it now; it is not shown again:"
            return out, f"{note}\n\n{out['secret']}"
        if action == "list":
            out = call("ListAPIKeys", {"project": args.project})
            rows = [["name", "roles", "source", "expires", "last used", ""]]
            for k in out.get("keys", []):
                roles = "; ".join(
                    f"{p}={','.join(r.get('roles', []))}"
                    for p, r in sorted(k.get("roles", {}).items())
                )
                rows.append(
                    [
                        k["name"],
                        roles,
                        k.get("source", ""),
                        k.get("expiresAt", "-"),
                        k.get("lastUsedAt", "-"),
                        "revoked" if k.get("revoked") else "",
                    ]
                )
            return out, rows
        return call("RevokeAPIKey", {"name": args.name}), f"revoked API key {args.name}"
    if area == "roles":
        if action == "list":
            out = call("ListRoles", {"project": args.project})
            rows = [["role", "project", "source", "permissions", "inherits", "condition"]]
            for r in out.get("roles", []):
                rows.append(
                    [
                        r["name"],
                        r.get("project", "") or "*",
                        r.get("source", ""),
                        ",".join(r.get("permissions", [])),
                        ",".join(r.get("inherits", [])),
                        r.get("condition", ""),
                    ]
                )
            return out, rows
        if action == "permissions":
            out = call("ListPermissions")
            rows = [["permission", "description"]]
            for p in out.get("permissions", []):
                note = " (owner only)" if p.get("installWide") else ""
                rows.append([p["name"], p.get("description", "") + note])
            return out, rows
        if action == "delete":
            out = call("DeleteRole", {"name": args.name, "project": args.project})
            return out, f"deleted role {args.name}"
        role = _role_body(args)
        out = call("CreateRole" if action == "create" else "UpdateRole", {"role": role})
        return out, f"{action}d role {role['name']}"
    if area == "bindings":
        if action == "list":
            out = call("ListBindings", {"project": args.project})
            rows = [["project", "role", "subject", "source"]]
            for b in out.get("bindings", []):
                rows.append([b["project"], b["role"], b["subject"], b.get("source", "")])
            return out, rows
        binding = {"project": args.project, "role": args.role, "subject": args.subject}
        if action == "create":
            out = call("CreateBinding", {"binding": binding})
            return out, f"granted {args.role} in {args.project} to {args.subject}"
        out = call("DeleteBinding", {"binding": binding})
        return out, f"removed {args.role} in {args.project} from {args.subject}"
    if area == "projects":
        if action == "create":
            out = call("CreateProject", {"name": args.name, "description": args.description})
            return out, f"created project {args.name}"
        out = call("ListProjects")
        rows = [["project", "source", "description"]]
        for p in out.get("projects", []):
            rows.append([p["name"], p.get("source", ""), p.get("description", "")])
        return out, rows
    out = call(
        "ListAuditEvents",
        {
            "project": args.project,
            "action": args.audit_action,
            "deniedOnly": args.denied,
            "pageSize": args.limit,
        },
    )
    rows = [["time", "principal", "action", "project", "resource", "decision", "reason"]]
    for ev in out.get("events", []):
        rows.append(
            [
                ev.get("time", ""),
                ev.get("principal", ""),
                ev.get("action", ""),
                ev.get("project", ""),
                ev.get("resource", ""),
                "allow" if ev.get("allowed") else "DENY",
                ev.get("reason", ""),
            ]
        )
    return out, rows
