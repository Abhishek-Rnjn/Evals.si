"""``evalsi guardrails``: inline guardrails on a server.

A guardrail redacts what its rules match and runs evaluators on content on
its way to or from a model or a tool; agentgateway calls it inline (see the
guardrails guide). ``check`` tries content against a stored guardrail, or a
guardrail file as a dry run, and exits 3 when the content would be blocked.
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any

from evalsi.cli import EXIT_GATES_FAILED, add_credential_args, server_client

_ENUMS = {
    "phases": "GUARDRAIL_PHASE_",
    "mode": "GUARDRAIL_MODE_",
    "failure_mode": "GUARDRAIL_FAILURE_MODE_",
}


def add_guardrail_commands(sub: Any) -> None:
    gr = sub.add_parser("guardrails", help="inline guardrails on a server")
    gr_sub = gr.add_subparsers(dest="action", required=True)

    def common(p: argparse.ArgumentParser) -> None:
        p.add_argument("--server", required=True, help="evalsid URL")
        add_credential_args(p)

    ap = gr_sub.add_parser("apply", help="create or replace a guardrail from a YAML or JSON file")
    ap.add_argument("-f", "--file", required=True)
    common(ap)
    ap.set_defaults(func=_cmd_apply)

    ls = gr_sub.add_parser("list", help="list guardrails")
    ls.add_argument("--project", default="")
    common(ls)
    ls.set_defaults(func=_cmd_list)

    rm = gr_sub.add_parser("delete", help="delete a guardrail")
    rm.add_argument("name")
    rm.add_argument("--project", default="")
    common(rm)
    rm.set_defaults(func=_cmd_delete)

    ck = gr_sub.add_parser(
        "check", help="check content against a guardrail (exit 3 when it would be blocked)"
    )
    which = ck.add_mutually_exclusive_group(required=True)
    which.add_argument("name", nargs="?", help="a stored guardrail")
    which.add_argument("-f", "--file", help="a guardrail file, checked without storing it")
    ck.add_argument("--project", default="")
    ck.add_argument("--phase", choices=["request", "response"], default="request")
    ck.add_argument("--text", help="the content (default: standard input)")
    ck.add_argument("--label", action="append", default=[], help="key=value, for block_when")
    ck.add_argument("--format", choices=["text", "json"], default="text")
    common(ck)
    ck.set_defaults(func=_cmd_check)


def guardrail_from_doc(doc: dict[str, Any]) -> dict[str, Any]:
    """A guardrail file (snake_case, short enum values, evaluator refs as strings)
    as GuardrailService JSON."""
    from google.protobuf import json_format

    from evalsi.v1alpha1 import guardrail_service_pb2 as pb

    doc = dict(doc)
    for key, prefix in _ENUMS.items():
        value = doc.get(key)
        if isinstance(value, list):
            doc[key] = [_enum(v, prefix) for v in value]
        elif isinstance(value, str):
            doc[key] = _enum(value, prefix)
    doc["evaluators"] = [
        {"ref": e} if isinstance(e, str) else e for e in doc.get("evaluators") or []
    ]
    msg = json_format.ParseDict(doc, pb.Guardrail())
    out: dict[str, Any] = json_format.MessageToDict(msg)
    return out


def _enum(value: str, prefix: str) -> str:
    return value if value.startswith(prefix) else prefix + value.upper().replace("-", "_")


def _load(path: str) -> dict[str, Any]:
    import yaml

    doc = yaml.safe_load(Path(path).read_text())
    if not isinstance(doc, dict):
        raise ValueError(f"{path}: expected a mapping")
    return guardrail_from_doc(doc)


def _cmd_apply(args: argparse.Namespace) -> int:
    with server_client(args) as client:
        g = client.apply_guardrail(_load(args.file))
    print(f"applied guardrail {g.get('project', '')}/{g['name']}")
    return 0


def _cmd_list(args: argparse.Namespace) -> int:
    with server_client(args) as client:
        for g in client.list_guardrails(args.project):
            mode = str(g.get("mode", "")).removeprefix("GUARDRAIL_MODE_").lower() or "enforce"
            print(f"{g.get('project', '')}/{g['name']}\t{mode}\t{g.get('description', '')}")
    return 0


def _cmd_delete(args: argparse.Namespace) -> int:
    with server_client(args) as client:
        client.delete_guardrail(args.project, args.name)
    print(f"deleted guardrail {args.name}")
    return 0


def _cmd_check(args: argparse.Namespace) -> int:
    content = args.text if args.text is not None else sys.stdin.read()
    labels = dict(kv.split("=", 1) for kv in args.label)
    with server_client(args) as client:
        out = client.check_guardrail(
            content,
            project=args.project,
            guardrail=args.name or "",
            inline=_load(args.file) if args.file else None,
            phase=args.phase,
            labels=labels,
        )
    decision = str(out.get("decision", "")).removeprefix("GUARDRAIL_DECISION_").lower()
    if args.format == "json":
        print(json.dumps(out, indent=2))
    else:
        print(f"{decision}: {out.get('reason', '')}".rstrip(": "))
        for metric, value in sorted(out.get("scores", {}).items()):
            print(f"  {metric} = {value:.3g}")
        if decision == "mask":
            print(f"\n{out.get('content', '')}")
    return EXIT_GATES_FAILED if decision == "block" else 0
