"""``evalsi credentials``: what a project's requests may use on a server.

Run specs, agents and evaluator params name secrets by variable
(``api_key_env``, ``headers_env``, ``env_from``), and a server checks each
name against the project's grants. ``list`` shows the grants that apply to a
project (names and the hosts values may go to, never values) and the judges
the project may use.
"""

from __future__ import annotations

import argparse
import json
from typing import Any

from evalsi.cli import add_credential_args, server_client


def add_credentials_commands(sub: Any) -> None:
    cr = sub.add_parser(
        "credentials", help="worker variables and judges a project may use on a server"
    )
    cr_sub = cr.add_subparsers(dest="action", required=True)
    ls = cr_sub.add_parser("list", help="list a project's credential grants and judges")
    ls.add_argument("--project", default="")
    ls.add_argument("--format", choices=["text", "json"], default="text")
    ls.add_argument("--server", required=True, help="evalsid URL")
    add_credential_args(ls)
    ls.set_defaults(func=_cmd_list)


def _cmd_list(args: argparse.Namespace) -> int:
    with server_client(args) as client:
        out = client.list_credentials(args.project)
    if args.format == "json":
        print(json.dumps(out, indent=2, sort_keys=True))
        return 0
    if not out.get("enforced"):
        print("grants are not enforced on this server: requests may name any worker variable")
    grants = out.get("grants") or []
    if not grants:
        print("no worker variables granted")
    for g in grants:
        hosts = ", ".join(g.get("hosts") or []) or "any host"
        notes = []
        if g.get("allowHttp"):
            notes.append("http allowed")
        if g.get("allProjects"):
            notes.append("every project")
        suffix = f"\t({', '.join(notes)})" if notes else ""
        print(f"{g['env']}\t{hosts}{suffix}")
    print("judges: " + (", ".join(out.get("judges") or []) or "none"))
    return 0
