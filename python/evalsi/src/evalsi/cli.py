"""The ``evalsi`` command line.

Phase 0 commands run fully in-process. ``evalsi serve`` arrives in Phase 1,
when it will start the Go daemon (``evalsid``).
"""

from __future__ import annotations

import argparse
import contextlib
import inspect
import json
import os
import sys
from collections.abc import Sequence
from dataclasses import asdict
from typing import Any

from evalsi._version import __version__
from evalsi.datasets import DatasetError
from evalsi.evaluator import EvaluatorConfigError
from evalsi.judges import PROVIDERS, JudgeConfig, JudgeError
from evalsi.registry import default_registry
from evalsi.runner import evaluate
from evalsi.types import Outcome

_EMPTY = inspect.Parameter.empty


def main(argv: Sequence[str] | None = None) -> int:
    argv = list(sys.argv[1:] if argv is None else argv)
    if argv and argv[0] == "serve" and not {"-h", "--help"} & set(argv[1:2]):
        # Everything after `serve` belongs to evalsid; argparse would try to parse its flags.
        argv = ["serve", "--", *argv[1:]]
    parser = _parser()
    args = parser.parse_args(argv)
    try:
        code: int = args.func(args)
        return code
    except (EvaluatorConfigError, DatasetError, JudgeError, ValueError, RuntimeError) as exc:
        # RuntimeError covers ServerError and AuthError.
        print(f"error: {exc}", file=sys.stderr)
        return 1


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="evalsi",
        description="Evaluate AI agents, RAG pipelines, LLMs and models, from CI to training.",
    )
    parser.add_argument("--version", action="version", version=f"evalsi {__version__}")
    sub = parser.add_subparsers(dest="command", required=True)

    _add_eval(sub)
    _add_run(sub)
    _add_matrix(sub)
    _add_gate(sub)
    _add_policy(sub)
    from evalsi.cli_analytics import add_analytics_commands
    from evalsi.cli_annotate import add_annotate_commands
    from evalsi.cli_auth import add_auth_commands
    from evalsi.cli_credentials import add_credentials_commands
    from evalsi.cli_flywheel import add_flywheel_commands
    from evalsi.cli_guardrails import add_guardrail_commands
    from evalsi.cli_mcp import add_mcp_command
    from evalsi.cli_plugins import add_plugin_commands
    from evalsi.cli_rewards import add_report_command, add_rewards_commands
    from evalsi.cli_sources import add_source_commands
    from evalsi.cli_traces import add_trace_commands
    from evalsi.cli_training import add_training_commands

    add_auth_commands(sub)
    add_flywheel_commands(sub)
    add_rewards_commands(sub)
    add_report_command(sub)
    add_training_commands(sub)
    add_analytics_commands(sub)
    add_mcp_command(sub)
    add_annotate_commands(sub)
    add_guardrail_commands(sub)
    add_source_commands(sub)
    add_trace_commands(sub)
    add_plugin_commands(sub)
    add_credentials_commands(sub)
    _add_diagnostic_commands(sub)
    cat = sub.add_parser("catalog", help="list installed evaluator packs and evaluators")
    cat.add_argument("--pack", help="only this pack")
    cat.add_argument("--format", choices=["table", "json"], default="table")
    cat.set_defaults(func=_cmd_catalog)

    wk = sub.add_parser(
        "worker", help="serve evaluators to evalsid over gRPC (needs evalsi[server])"
    )
    wk.add_argument("--listen", required=True, help="unix:///path/to.sock or host:port")
    wk.add_argument("--judges", help="JSON file mapping judge names to judge configs")
    wk.add_argument("--no-cache", action="store_true", help="do not reuse judge responses")
    wk.set_defaults(func=_cmd_worker)

    srv = sub.add_parser(
        "serve",
        help="run the Evals.si server (starts the evalsid binary)",
        add_help=False,
    )
    srv.add_argument("args", nargs=argparse.REMAINDER, help="arguments passed to evalsid serve")
    srv.set_defaults(func=_cmd_serve)

    where = sub.add_parser(
        "evalsid-path",
        help="print the evalsid this install uses (EVALSID, the platform wheel's, else PATH)",
    )
    where.set_defaults(func=_cmd_evalsid_path)

    ver = sub.add_parser("version", help="print the version")
    ver.set_defaults(func=_cmd_version)
    return parser


def _add_diagnostic_commands(sub: Any) -> None:
    from evalsi.cli_canaries import add_canary_commands
    from evalsi.cli_determinism import add_determinism_command

    add_canary_commands(sub)
    add_determinism_command(sub)


def _add_eval(sub: Any) -> None:
    ev = sub.add_parser("eval", help="score records you already have")
    ev.add_argument("--data", required=True, help="JSONL/JSON file or hf://repo?config=..&split=..")
    ev.add_argument(
        "--evaluators",
        required=True,
        help="comma-separated evaluator references, e.g. exact-match,llm-judge",
    )
    ev.add_argument(
        "--map",
        action="append",
        default=[],
        metavar="FIELD=PATH",
        help="map a record field to a column, e.g. --map input=question (repeatable)",
    )
    ev.add_argument(
        "--param",
        action="append",
        default=[],
        metavar="EVALUATOR.KEY=VALUE",
        help="evaluator param; VALUE is parsed as JSON when possible (repeatable)",
    )
    ev.add_argument("--limit", type=int, help="evaluate only the first N records")
    ev.add_argument("--concurrency", type=int, default=16)

    _add_judge_args(ev)

    stats = ev.add_argument_group("statistics")
    stats.add_argument("--confidence", type=float, default=0.95)
    stats.add_argument(
        "--cluster-by", help="metadata key grouping correlated records (clustered errors)"
    )
    stats.add_argument("--ci", choices=["auto", "bootstrap"], default="auto")

    out = ev.add_argument_group("output")
    out.add_argument("--output", help="write manifest, summaries and every result as JSON")
    out.add_argument("--format", choices=["table", "json"], default="table")
    out.add_argument("--quiet", action="store_true", help="no progress output")
    ev.set_defaults(func=_cmd_eval)


def _add_run(sub: Any) -> None:
    rn = sub.add_parser("run", help="execute a run spec (run.yaml), embedded or on a server")
    rn.add_argument("-f", "--file", required=True, help="the run spec")
    rn.add_argument(
        "--server", help="evalsid URL, e.g. http://localhost:8080; embedded when omitted"
    )
    rn.add_argument("--concurrency", type=int, default=8)
    rn.add_argument("--output", help="embedded: write manifest, summaries and results as JSON")
    rn.add_argument("--format", choices=["table", "json"], default="table")
    rn.add_argument("--quiet", action="store_true", help="no progress output")
    rn.add_argument("--no-wait", action="store_true", help="server: print the run id and exit")
    add_credential_args(rn)
    _add_judge_args(rn)
    rn.set_defaults(func=_cmd_run)

    cmp = sub.add_parser("compare", help="paired comparison of two server runs")
    cmp.add_argument("baseline")
    cmp.add_argument("candidate")
    cmp.add_argument("--server", required=True, help="evalsid URL")
    cmp.add_argument("--format", choices=["table", "json"], default="table")
    add_credential_args(cmp)
    cmp.set_defaults(func=_cmd_compare)


def _add_matrix(sub: Any) -> None:
    mx = sub.add_parser(
        "matrix",
        help="harness-by-model matrix over results files, with cost beside each score",
    )
    mx.add_argument("results", nargs="+", help="results files from `evalsi run --output`")
    mx.add_argument("--metric", required=True)
    mx.add_argument(
        "--tuned-on",
        action="append",
        default=[],
        help="a results file name, run name or dataset hash the harness was tuned on; "
        "cells are then split into tuned and held-out",
    )
    mx.add_argument(
        "--mechanisms",
        action="store_true",
        help="also show, per cell, which optional tools and context strategy ran in how many tasks",
    )
    mx.add_argument(
        "--gate",
        action="store_true",
        help="regression gate: exit 1 when a cell is significantly worse than the first cell "
        "(the paired difference's interval lies wholly on the bad side) or, with --tuned-on, "
        "a held-out cell is significantly worse than the baseline on held-out data or than "
        "its tuned cell",
    )
    mx.add_argument(
        "--max-regression",
        type=float,
        default=0.0,
        help="with --gate: tolerated drop in the metric; a cell fails only when its whole "
        "interval is worse than this (default 0)",
    )
    mx.add_argument(
        "--gate-strict",
        action="store_true",
        help="with --gate: also exit 1 when a cell has insufficient data to decide "
        "(by default it is reported, not failed)",
    )
    mx.add_argument("--format", choices=["table", "json"], default="table")
    mx.set_defaults(func=_cmd_matrix)

    ab = sub.add_parser(
        "ablate", help="component ablation of an adopted harness (H7): plan reverts, attribute"
    )
    ab_sub = ab.add_subparsers(dest="action", required=True)
    plan = ab_sub.add_parser(
        "plan", help="write one revert variant per edited component of an adopted harness"
    )
    plan.add_argument("base", help="the base harness directory")
    plan.add_argument("adopted", help="the adopted harness directory")
    plan.add_argument("--out", required=True, help="directory for the variants and manifest.json")
    plan.set_defaults(func=_cmd_ablate_plan)
    attr = ab_sub.add_parser(
        "attribute", help="per-component effect (full minus revert) from results files"
    )
    attr.add_argument("--metric", required=True)
    attr.add_argument("--base", required=True, help="results file for the base harness")
    attr.add_argument("--full", required=True, help="results file for the full adopted harness")
    attr.add_argument(
        "--variant",
        action="append",
        default=[],
        metavar="COMPONENT=FILE",
        help="results file for the variant with that component reverted (repeatable)",
    )
    attr.add_argument("--confidence", type=float, default=0.95)
    attr.add_argument("--format", choices=["table", "json"], default="table")
    attr.set_defaults(func=_cmd_ablate_attribute)

    delete = sub.add_parser("delete-run", help="delete a finished server run and its stored data")
    delete.add_argument("id")
    delete.add_argument("--server", required=True)
    delete.add_argument("--yes", action="store_true", required=True)
    delete.add_argument("--idempotency-key")
    add_credential_args(delete)
    delete.set_defaults(func=_cmd_delete_run)

    archive = sub.add_parser(
        "archive-run", help="drop a finished server run's records and results, keeping its summary"
    )
    archive.add_argument("id")
    archive.add_argument("--server", required=True)
    archive.add_argument("--yes", action="store_true", required=True)
    add_credential_args(archive)
    archive.set_defaults(func=_cmd_archive_run)

    dataset = sub.add_parser("dataset", help="upload or import server datasets")
    actions = dataset.add_subparsers(dest="action", required=True)
    for action in ("upload", "import"):
        command = actions.add_parser(action)
        command.add_argument("source")
        command.add_argument("--name", required=True)
        command.add_argument("--project", required=True)
        command.add_argument("--format", choices=["jsonl", "parquet"])
        command.add_argument("--server", required=True)
        add_credential_args(command)
        command.set_defaults(func=_cmd_dataset)


def _add_gate(sub: Any) -> None:
    gate = sub.add_parser(
        "gate",
        help="a deploy gate: run a spec on a server for one release, exit 0 pass, 3 fail",
        description=(
            "Run a spec on a server as the gate for one release of what it evaluates, "
            "wait for it, and exit 0 when its gates pass, 3 when one fails and 1 on an "
            "error. A release is gated once: running the same release again (a pipeline "
            "retry) returns the same run, and the same release with a changed spec is "
            "refused, for 24 hours (the server's idempotency window)."
        ),
    )
    gate.add_argument("-f", "--file", required=True, help="the run spec")
    gate.add_argument("--server", required=True, help="evalsid URL")
    gate.add_argument(
        "--release", required=True, help="the release being gated, e.g. v7 or a commit"
    )
    gate.add_argument(
        "--target-url",
        help="the candidate's URL: replaces target.agent.http.url or target.http.url",
    )
    gate.add_argument(
        "--set",
        action="append",
        default=[],
        metavar="PATH=VALUE",
        help="set a spec field by dotted path (VALUE is YAML), e.g. trials=5; repeatable",
    )
    gate.add_argument("--format", choices=["table", "json"], default="table")
    gate.add_argument("--quiet", action="store_true", help="no progress output")
    add_credential_args(gate)
    gate.set_defaults(func=_cmd_gate, no_wait=False)


def _cmd_delete_run(args: argparse.Namespace) -> int:
    from evalsi.client import ServerError

    try:
        with server_client(args) as client:
            client.delete_run(args.id, idempotency_key=args.idempotency_key)
    except ServerError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1
    print(f"deleted: {args.id}")
    return 0


def _cmd_archive_run(args: argparse.Namespace) -> int:
    from evalsi.client import ServerError

    try:
        with server_client(args) as client:
            run = client.archive_run(args.id)
    except ServerError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1
    print(f"archived: {args.id} ({run.get('archivedAt', '')})")
    return 0


def _cmd_dataset(args: argparse.Namespace) -> int:
    from pathlib import Path

    from evalsi.client import ServerError

    try:
        with server_client(args) as client:
            if args.action == "upload":
                result = client.upload_dataset(
                    args.source, name=args.name, project=args.project, format=args.format
                )
            else:
                encoding = args.format or Path(args.source).suffix.removeprefix(".")
                result = client.import_dataset(
                    args.source, name=args.name, project=args.project, format=encoding
                )
    except (ServerError, OSError) as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1
    print(f"{result['path']} ({result.get('records', 0)} records)")
    return 0


def _add_policy(sub: Any) -> None:
    pol = sub.add_parser("policy", help="manage online evaluation policies on a server")
    pol_sub = pol.add_subparsers(dest="action", required=True)
    pa = pol_sub.add_parser("apply", help="create or replace a policy from a YAML file")
    pa.add_argument("-f", "--file", required=True)
    pl = pol_sub.add_parser("list", help="list policies")
    pl.add_argument("--project", default="")
    ps = pol_sub.add_parser("stats", help="counters, window means and alerts of a policy")
    ps.add_argument("name")
    pd = pol_sub.add_parser("delete", help="delete a policy")
    pd.add_argument("name")
    for p in (pa, pl, ps, pd):
        p.add_argument("--server", required=True, help="evalsid URL")
        add_credential_args(p)
    pol.set_defaults(func=_cmd_policy)


def add_credential_args(parser: argparse.ArgumentParser) -> None:
    """--token and --api-key for commands that talk to a server."""
    creds = parser.add_argument_group(
        "credentials (default: EVALSI_API_KEY, EVALSI_TOKEN, GitHub Actions OIDC, evalsi login)"
    )
    creds.add_argument("--token", help="a bearer token (JWT) for the server")
    creds.add_argument("--api-key", help="an evalsid API key (evk_...)")


def server_client(args: argparse.Namespace) -> Any:
    """A client for args.server with the command's credentials."""
    from evalsi.client import Client

    return Client(
        args.server, token=getattr(args, "token", None), api_key=getattr(args, "api_key", None)
    )


def _add_judge_args(parser: argparse.ArgumentParser) -> None:
    judge = parser.add_argument_group("judge (or set EVALSI_JUDGE_* environment variables)")
    judge.add_argument("--judge-provider", choices=PROVIDERS)
    judge.add_argument("--judge-model")
    judge.add_argument("--judge-base-url")
    judge.add_argument("--judge-api-key-env", help="environment variable holding the API key")
    judge.add_argument("--judge-effort", help="anthropic only: low, medium, high, xhigh or max")
    judge.add_argument("--judge-max-tokens", type=int)
    judge.add_argument("--no-cache", action="store_true", help="do not reuse judge responses")


def _plural(count: int, noun: str) -> str:
    return f"{count} {noun}" if count == 1 else f"{count} {noun}s"


def _cmd_worker(args: argparse.Namespace) -> int:
    import asyncio
    import logging

    try:
        from evalsi import worker
    except ImportError as exc:
        raise ValueError(f"the worker needs: pip install 'evalsi[server]' ({exc})") from exc

    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
    judges = worker.load_judges(args.judges)
    with contextlib.suppress(KeyboardInterrupt):
        asyncio.run(worker.serve(args.listen, judges=judges, cache=not args.no_cache))
    return 0


def _cmd_serve(args: argparse.Namespace) -> int:
    from evalsi._binaries import evalsid_path

    binary = evalsid_path()
    if not binary:
        raise ValueError(
            "evalsid is not installed. Install a platform wheel of evalsi (Linux, amd64 or "
            "arm64), or build it with `go build ./cmd/evalsid` and put it on PATH, or set "
            "EVALSID to its path."
        )
    passthrough = [a for a in args.args if a != "--"] if args.args[:1] == ["--"] else args.args
    os.execv(binary, [binary, "serve", *passthrough])
    return 0  # pragma: no cover - execv does not return


def _cmd_evalsid_path(_args: argparse.Namespace) -> int:
    from evalsi._binaries import evalsid_path

    binary = evalsid_path()
    if not binary:
        print("error: no evalsid: install a platform wheel, or set EVALSID", file=sys.stderr)
        return 1
    print(binary)
    return 0


def _cmd_version(_args: argparse.Namespace) -> int:
    print(f"evalsi {__version__}")
    return 0


def _split_pairs(items: Sequence[str], flag: str) -> list[tuple[str, str]]:
    pairs = []
    for item in items:
        key, sep, value = item.partition("=")
        if not sep or not key:
            raise ValueError(f"{flag} expects KEY=VALUE, got {item!r}")
        pairs.append((key.strip(), value))
    return pairs


def _parse_value(raw: str) -> Any:
    try:
        return json.loads(raw)
    except json.JSONDecodeError:
        return raw


def _params(items: Sequence[str]) -> dict[str, dict[str, Any]]:
    params: dict[str, dict[str, Any]] = {}
    for key, value in _split_pairs(items, "--param"):
        name, dot, param = key.partition(".")
        if not dot or not param:
            raise ValueError(f"--param expects EVALUATOR.KEY=VALUE, got {key!r}")
        params.setdefault(name, {})[param] = _parse_value(value)
    return params


def _judge(args: argparse.Namespace) -> JudgeConfig | None:
    base = JudgeConfig.from_env()
    fields: dict[str, Any] = {}
    if base is not None:
        fields = {
            k: getattr(base, k)
            for k in ("provider", "model", "base_url", "api_key_env", "effort", "max_tokens")
        }
    overrides = {
        "provider": args.judge_provider,
        "model": args.judge_model,
        "base_url": args.judge_base_url,
        "api_key_env": args.judge_api_key_env,
        "effort": args.judge_effort,
        "max_tokens": args.judge_max_tokens,
    }
    fields.update({k: v for k, v in overrides.items() if v is not None})
    if not fields.get("model"):
        return None
    fields.setdefault("provider", "openai-compatible")
    return JudgeConfig(**fields)


def _cmd_eval(args: argparse.Namespace) -> int:
    refs = [r.strip() for r in args.evaluators.split(",") if r.strip()]
    show_progress = not args.quiet and sys.stderr.isatty()

    def progress(done: int, total: int) -> None:
        print(f"\revaluating {done}/{total}", end="", file=sys.stderr, flush=True)

    result = evaluate(
        args.data,
        refs,
        mapping=dict(_split_pairs(args.map, "--map")),
        params=_params(args.param),
        limit=args.limit,
        judge=_judge(args),
        cache=not args.no_cache,
        concurrency=args.concurrency,
        confidence=args.confidence,
        cluster_by=args.cluster_by,
        ci_method=args.ci,
        on_progress=progress if show_progress else None,
    )
    if show_progress:
        print(file=sys.stderr)
    if args.output:
        result.save(args.output)
    if args.format == "json":
        payload = {
            "manifest": result.manifest,
            "summaries": [s.to_dict() for s in result.summaries],
        }
        print(json.dumps(payload, indent=2, default=str))
    else:
        judge = result.manifest["judge"]
        judge_note = f" · judge {judge['provider']}/{judge['model']}" if judge else ""
        print(
            f"{_plural(len(result.records), 'record')} · "
            f"{_plural(len(result.manifest['evaluators']), 'evaluator')}"
            f"{judge_note}\n"
        )
        print(result.table())
    few = sorted(
        {s.clusters for s in result.summaries if s.clusters is not None and s.clusters < 10}
    )
    if few and not args.quiet:
        print(
            f"\nnote: --cluster-by {args.cluster_by} gives only {few[0]} cluster(s) for some "
            "metrics; their intervals are wide and approximate.",
            file=sys.stderr,
        )
    errors = result.errors()
    if errors:
        print(
            f"\n{len(errors)} evaluation(s) errored and are excluded from the metrics.",
            file=sys.stderr,
        )
        for err in errors[:5]:
            print(f"  record {err.record_id!r} · {err.evaluator}: {err.reason}", file=sys.stderr)
    skipped = sum(r.outcome is Outcome.SKIPPED for r in result.results)
    if skipped and args.format == "table" and not args.quiet:
        print(f"\n{skipped} evaluation(s) skipped; --output records why.", file=sys.stderr)
    if args.output and not args.quiet:
        print(f"\nfull results: {os.path.abspath(args.output)}", file=sys.stderr)
    return 0


def _cmd_catalog(args: argparse.Namespace) -> int:
    registry = default_registry()
    packs = [p for p in registry.packs.values() if args.pack in (None, p.name)]
    if args.pack and not packs:
        raise ValueError(f"no pack named {args.pack!r}; installed: {sorted(registry.packs)}")
    if args.format == "json":
        print(
            json.dumps(
                [
                    {
                        "pack": p.name,
                        "description": p.description,
                        "tier": p.tier or "community",
                        "on_by_default": p.on_by_default,
                        "evaluators": [
                            {
                                "name": d.spec.name,
                                "version": d.spec.version,
                                "runtime": d.spec.runtime,
                                "description": d.spec.description,
                                "params": {
                                    k: None if v is _EMPTY else v for k, v in d.spec.params.items()
                                },
                            }
                            for d in p.evaluators
                        ],
                    }
                    for p in packs
                ],
                indent=2,
                default=str,
            )
        )
        return 0
    for pack in packs:
        default = ", on by default" if pack.on_by_default else ""
        runtime = ", wasm" if pack.evaluators and pack.evaluators[0].spec.runtime == "wasm" else ""
        print(f"{pack.name} ({pack.tier or 'community'}{runtime}{default}): {pack.description}")
        for d in pack.evaluators:
            params = ", ".join(k if v is _EMPTY else f"{k}={v!r}" for k, v in d.spec.params.items())
            print(f"  {d.spec.name}@{d.spec.version}  {d.spec.description}")
            if params:
                print(f"      params: {params}")
        print()
    return 0


EXIT_GATES_FAILED = 3


def _print_gates(gates: list[dict[str, Any]]) -> None:
    if not gates:
        return
    print("\ngates:")
    for g in gates:
        mark = "pass" if g.get("passed") else "FAIL"
        bound = " ".join(f"{k} {g[k]}" for k in ("min", "max") if g.get(k) is not None)
        detail = f" ({g['reason']})" if g.get("reason") else ""
        print(f"  [{mark}] {g.get('metric')} {g.get('stat', 'mean')} {bound}{detail}")


def _cmd_run(args: argparse.Namespace) -> int:
    from evalsi.runspec import load_spec

    run_file = load_spec(args.file)
    if args.server:
        return _run_on_server(args, run_file)

    import asyncio

    from evalsi.run import BudgetExceeded, execute

    show = not args.quiet and sys.stderr.isatty()

    def progress(stage: str, done: int, total: int) -> None:
        print(f"\r{stage}: {done}/{total}   ", end="", file=sys.stderr, flush=True)

    try:
        outcome = asyncio.run(
            execute(
                run_file,
                judge=_judge(args),
                cache=not args.no_cache,
                concurrency=args.concurrency,
                on_progress=progress if show else None,
            )
        )
    except BudgetExceeded as exc:
        print(f"error: budget exceeded: {exc}", file=sys.stderr)
        return 1
    if show:
        print(file=sys.stderr)
    result = outcome.result
    if args.output:
        payload = result.to_dict()
        payload["gates"] = [g.to_dict() for g in outcome.gates]
        with open(args.output, "w", encoding="utf-8") as handle:
            json.dump(payload, handle, indent=2, default=str)
    gates = [g.to_dict() for g in outcome.gates]
    if args.format == "json":
        print(
            json.dumps(
                {
                    "manifest": result.manifest,
                    "summaries": [s.to_dict() for s in result.summaries],
                    "gates": gates,
                },
                indent=2,
                default=str,
            )
        )
    else:
        trials = result.manifest["trials"]
        note = f" x {trials} trials" if trials > 1 else ""
        print(f"{_plural(result.manifest['dataset']['records'], 'record')}{note}\n")
        print(result.table())
        _print_gates(gates)
    errors = result.errors()
    if result.all_errored:
        # Nothing was scored: a run without gates must not pass.
        reason = errors[0].reason or "no reason given"
        print(
            f"\nerror: all {len(errors)} evaluations failed; the first: {reason}",
            file=sys.stderr,
        )
        return 1
    if errors and not args.quiet:
        print(
            f"\n{len(errors)} evaluation(s) errored and are excluded from the metrics.",
            file=sys.stderr,
        )
    return 0 if outcome.passed else EXIT_GATES_FAILED


def summary_from_json(s: dict[str, Any]) -> Any:
    """A metric summary as the server's JSON gives it. Proto3 JSON leaves out
    zeros, so an interval from 0 has no ``low`` and a mean of 0 has no ``mean``."""
    from evalsi.results import MetricSummary
    from evalsi.stats import Interval

    ci = s.get("ci")
    return MetricSummary(
        metric=s["metric"],
        evaluator=s.get("evaluator", ""),
        kind=str(s.get("kind", "")).removeprefix("METRIC_KIND_").lower(),
        n=int(s.get("n", 0)),
        mean=s.get("mean", 0.0) if int(s.get("n", 0)) else s.get("mean"),
        std=s.get("std"),
        ci=Interval(
            float(ci.get("low", 0.0)),
            float(ci.get("high", 0.0)),
            float(ci.get("level", 0.0)),
            str(ci.get("method", "")),
        )
        if ci
        else None,
        skipped=int(s.get("skipped", 0)),
        errors=int(s.get("errors", 0)),
        labels={k: int(v) for k, v in s.get("labels", {}).items()},
    )


def _set_path(doc: dict[str, Any], path: str, value: Any) -> None:
    node = doc
    *parents, last = path.split(".")
    for key in parents:
        child = node.get(key)
        if not isinstance(child, dict):
            child = node[key] = {}
        node = child
    node[last] = value


def gated_spec(
    spec: dict[str, Any], *, target_url: str | None, sets: Sequence[str]
) -> dict[str, Any]:
    """The spec with the gate's overrides applied (a copy)."""
    import copy

    import yaml

    out = copy.deepcopy(spec)
    if target_url:
        target = out.get("target") or {}
        http = (target.get("agent") or {}).get("http") or target.get("http")
        if not isinstance(http, dict):
            raise ValueError("--target-url needs an HTTP target (target.agent.http or target.http)")
        http["url"] = target_url
    for item in sets:
        path, sep, raw = item.partition("=")
        if not sep or not path:
            raise ValueError(f"--set takes PATH=VALUE, not {item!r}")
        _set_path(out, path, yaml.safe_load(raw))
    return out


def gate_key(project: str, name: str, release: str) -> str:
    """The idempotency key a release's gate run is created with."""
    import hashlib

    raw = "\x00".join(["evalsi-gate", project, name, release]).encode()
    return "gate-" + hashlib.sha256(raw).hexdigest()[:48]


def _cmd_gate(args: argparse.Namespace) -> int:
    from dataclasses import replace

    from google.protobuf import json_format

    from evalsi.runspec import load_spec, spec_to_dict
    from evalsi.v1alpha1 import run_pb2

    run_file = load_spec(args.file)
    spec = gated_spec(spec_to_dict(run_file.spec), target_url=args.target_url, sets=args.set)
    try:
        message = json_format.ParseDict(spec, run_pb2.RunSpec())
    except json_format.ParseError as exc:
        raise ValueError(f"{args.file}: the spec after --set is invalid: {exc}") from exc
    run_file = replace(run_file, spec=message, labels={**run_file.labels, "release": args.release})
    return _run_on_server(
        args,
        run_file,
        idempotency_key=gate_key(run_file.project, run_file.name, args.release),
        release=args.release,
    )


def _run_on_server(
    args: argparse.Namespace,
    run_file: Any,
    *,
    idempotency_key: str | None = None,
    release: str = "",
) -> int:
    from evalsi.client import ServerError
    from evalsi.results import EvalResult
    from evalsi.runspec import spec_to_dict

    with server_client(args) as client:
        try:
            run = client.create_run(
                spec_to_dict(run_file.spec),
                name=run_file.name,
                project=run_file.project,
                labels=run_file.labels,
                idempotency_key=idempotency_key,
            )
            if args.no_wait:
                print(run["id"])
                return 0
            show = not args.quiet and sys.stderr.isatty()
            for event in client.watch_run(run["id"]):
                if "run" in event:
                    run = event["run"]
                elif "progress" in event and show:
                    p = event["progress"]
                    print(
                        f"\r{run['id']}: {p.get('done', 0)}/{p.get('total', 0)}   ",
                        end="",
                        file=sys.stderr,
                        flush=True,
                    )
            if show:
                print(file=sys.stderr)
        except ServerError as exc:
            if release and exc.code == "already_exists":
                print(
                    f"error: release {release} was already gated with a different spec; "
                    "gate it as a new release",
                    file=sys.stderr,
                )
            else:
                print(f"error: {exc}", file=sys.stderr)
            return 1
    status = run.get("status", "")
    if args.format == "json":
        print(json.dumps(run, indent=2))
    else:
        summaries = [summary_from_json(s) for s in run.get("summaries", [])]
        print(f"run {run['id']}: {status.removeprefix('RUN_STATUS_').lower()}\n")
        print(EvalResult(records=[], results=[], summaries=summaries, manifest={}).table())
        _print_gates(
            [
                {
                    **g.get("gate", {}),
                    "stat": str(g.get("gate", {}).get("stat", "mean"))
                    .removeprefix("GATE_STAT_")
                    .lower(),
                    "passed": g.get("passed", False),
                    "reason": g.get("reason", ""),
                }
                for g in run.get("gates", [])
            ]
        )
        if run.get("error"):
            print(f"\nerror: {run['error']}", file=sys.stderr)
    if status == "RUN_STATUS_SUCCEEDED":
        return 0
    if status == "RUN_STATUS_FAILED":
        return EXIT_GATES_FAILED
    return 1


def load_policy(path: str) -> dict[str, Any]:
    """Read a policy file into the OnlineEvalPolicy JSON form, validated by the proto."""
    import yaml
    from google.protobuf import json_format

    from evalsi.runspec import api_labels, normalize_durations, project_of
    from evalsi.v1alpha1 import monitor_service_pb2

    with open(path, encoding="utf-8") as handle:
        document = yaml.safe_load(handle)
    if not isinstance(document, dict) or not isinstance(document.get("spec"), dict):
        raise ValueError(f"{path}: expected a mapping with a 'spec'")
    if document.get("kind", "OnlineEvalPolicy") != "OnlineEvalPolicy":
        raise ValueError(f"{path}: kind must be OnlineEvalPolicy")
    metadata = document.get("metadata") or {}
    body = {
        **document["spec"],
        "name": metadata.get("name", ""),
        "project": project_of(metadata),
        "labels": api_labels(metadata.get("labels") or {}),
    }
    try:
        message = json_format.ParseDict(
            normalize_durations(body, monitor_service_pb2.OnlineEvalPolicy.DESCRIPTOR),
            monitor_service_pb2.OnlineEvalPolicy(),
        )
    except json_format.ParseError as exc:
        raise ValueError(f"{path}: invalid policy: {exc}") from exc
    out: dict[str, Any] = json_format.MessageToDict(message)
    return out


def _cmd_policy(args: argparse.Namespace) -> int:
    from evalsi.client import ServerError

    with server_client(args) as client:
        try:
            if args.action == "apply":
                policy = load_policy(args.file)
                client.call("MonitorService", "ApplyPolicy", {"policy": policy})
                print(f"applied policy {policy['name']}")
            elif args.action == "list":
                out = client.call("MonitorService", "ListPolicies", {"project": args.project})
                for p in out.get("policies", []):
                    state = " (disabled)" if p.get("disabled") else ""
                    print(f"{p['name']}{state}  {p.get('selector', '')}")
            elif args.action == "stats":
                stats = client.call("MonitorService", "GetPolicyStats", {"name": args.name})[
                    "stats"
                ]
                print(json.dumps(stats, indent=2))
            else:
                client.call("MonitorService", "DeletePolicy", {"name": args.name})
                print(f"deleted policy {args.name}")
        except ServerError as exc:
            print(f"error: {exc}", file=sys.stderr)
            return 1
    return 0


def _cmd_matrix(args: argparse.Namespace) -> int:
    from evalsi import matrix

    loaded = []
    for path in args.results:
        try:
            with open(path, encoding="utf-8") as handle:
                loaded.append((os.path.basename(path), json.load(handle)))
        except (OSError, json.JSONDecodeError) as exc:
            print(f"error: {path}: {exc}", file=sys.stderr)
            return 2
    try:
        cells = matrix.build(loaded, args.metric, tuned_on=args.tuned_on)
    except ValueError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 2
    if (args.max_regression or args.gate_strict) and not args.gate:
        print("error: --max-regression and --gate-strict need --gate", file=sys.stderr)
        return 2
    verdicts = []
    if args.gate:
        verdicts = matrix.gate(loaded, cells, args.metric, max_regression=args.max_regression)
    if args.format == "json":
        rows = [c.to_dict() for c in cells]
        if args.mechanisms:
            for row, (_, data) in zip(rows, loaded, strict=True):
                row["mechanisms"] = [asdict(m) for m in matrix.mechanisms(data)]
        for row, verdict in zip(rows, verdicts, strict=False):
            row["gate"] = verdict.to_dict()
        print(json.dumps(rows, indent=2))
    else:
        print(matrix.table(cells))
        if args.mechanisms:
            print("\n" + matrix.mechanism_lines(loaded))
        if verdicts:
            print("\n" + matrix.gate_lines(verdicts, strict=args.gate_strict))
    if verdicts and matrix.gate_failed(verdicts, strict=args.gate_strict):
        print("error: the regression gate failed", file=sys.stderr)
        return 1
    return 0


def _cmd_ablate_plan(args: argparse.Namespace) -> int:
    from evalsi import ablation

    try:
        manifest = ablation.plan_dirs(args.base, args.adopted, args.out)
    except ablation.AblationError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 2
    print(json.dumps(manifest, indent=2))
    return 0


def _cmd_ablate_attribute(args: argparse.Namespace) -> int:
    from evalsi import ablation

    def load(path: str) -> Any:
        with open(path, encoding="utf-8") as handle:
            return json.load(handle)

    try:
        variants = {}
        for item in args.variant:
            component, sep, path = item.partition("=")
            if not sep or not component or not path:
                raise ablation.AblationError(f"--variant wants COMPONENT=FILE, got {item!r}")
            variants[component] = load(path)
        attributions = ablation.attribute(
            load(args.base), load(args.full), variants, args.metric, level=args.confidence
        )
    except (OSError, json.JSONDecodeError, ablation.AblationError) as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 2
    if args.format == "json":
        print(json.dumps([a.to_dict() for a in attributions], indent=2))
    else:
        print(ablation.attribution_table(attributions))
    return 0


def _fmt3(value: float | None) -> str:
    return "-" if value is None else f"{value:.3f}"


def _cmd_compare(args: argparse.Namespace) -> int:
    with server_client(args) as client:
        comparisons = client.compare_runs(args.baseline, args.candidate)
    if args.format == "json":
        print(json.dumps(comparisons, indent=2))
        return 0
    _cmd_compare_rows(comparisons)
    return 0


def _cmd_compare_rows(comparisons: list[dict[str, Any]]) -> None:
    rows = [["metric", "n", "baseline", "candidate", "diff", "CI", ""]]
    for c in comparisons:
        ci = c.get("diffCi")
        rows.append(
            [
                c["metric"],
                str(c.get("pairedN", 0)),
                _fmt3(c.get("baselineMean")),
                _fmt3(c.get("candidateMean")),
                _fmt3(c.get("diff")),
                f"[{ci['low']:.3f}, {ci['high']:.3f}]" if ci else "-",
                "significant" if c.get("significant") else "",
            ]
        )
    widths = [max(len(r[i]) for r in rows) for i in range(len(rows[0]))]
    for row in rows:
        print("  ".join(cell.ljust(w) for cell, w in zip(row, widths, strict=True)).rstrip())


if __name__ == "__main__":  # pragma: no cover
    sys.exit(main())
