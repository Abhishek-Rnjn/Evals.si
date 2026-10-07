"""``evalsi mcp``: serve Evals.si to MCP clients (coding agents) over stdio."""

from __future__ import annotations

import argparse
import os
from pathlib import Path
from typing import Any

from evalsi.cli import add_credential_args


def add_mcp_command(sub: Any) -> None:
    from evalsi.cli import _add_judge_args

    mc = sub.add_parser(
        "mcp",
        help="serve Evals.si to MCP clients (coding agents) over stdio",
        description=(
            "An MCP server on stdin/stdout with the tools list_evaluators, evaluate, run and "
            "compare. Configure it in your agent as the command `evalsi mcp`."
        ),
    )
    mc.add_argument(
        "--root",
        default=".",
        help="the workspace: run specs and results files must be inside it (default: .)",
    )
    mc.add_argument(
        "--results-dir",
        default=".evalsi/results",
        help="where run results are saved, inside the root (default: .evalsi/results)",
    )
    mc.add_argument("--server", help="run on this evalsid instead of embedded")
    mc.add_argument("--project", default="", help="server: the default project")
    mc.add_argument("--concurrency", type=int, default=8)
    add_credential_args(mc)
    _add_judge_args(mc)
    mc.set_defaults(func=_cmd_mcp)


def _cmd_mcp(args: argparse.Namespace) -> int:
    import asyncio

    from evalsi.cli import _judge
    from evalsi.mcp_server import EvalsiMCP, Settings, serve_stdio

    root = Path(args.root).resolve()
    if not root.is_dir():
        raise ValueError(f"--root {args.root} is not a directory")
    os.chdir(root)
    settings = Settings(
        root=root,
        server=args.server,
        project=args.project,
        token=args.token,
        api_key=args.api_key,
        judge=_judge(args),
        cache=not args.no_cache,
        concurrency=args.concurrency,
        results_dir=args.results_dir,
    )
    asyncio.run(serve_stdio(EvalsiMCP(settings)))
    return 0
