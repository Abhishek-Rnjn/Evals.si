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
    except (EvaluatorConfigError, DatasetError, JudgeError, ValueError) as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="evalsi", description="Evaluate ML models, LLMs and agents."
    )
    parser.add_argument("--version", action="version", version=f"evalsi {__version__}")
    sub = parser.add_subparsers(dest="command", required=True)

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

    judge = ev.add_argument_group("judge (or set EVALSI_JUDGE_* environment variables)")
    judge.add_argument("--judge-provider", choices=PROVIDERS)
    judge.add_argument("--judge-model")
    judge.add_argument("--judge-base-url")
    judge.add_argument("--judge-api-key-env", help="environment variable holding the API key")
    judge.add_argument("--judge-effort", help="anthropic only: low, medium, high, xhigh or max")
    judge.add_argument("--judge-max-tokens", type=int)
    judge.add_argument("--no-cache", action="store_true", help="do not reuse judge responses")

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

    ver = sub.add_parser("version", help="print the version")
    ver.set_defaults(func=_cmd_version)
    return parser


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
    import shutil

    binary = os.environ.get("EVALSID") or shutil.which("evalsid")
    if not binary:
        raise ValueError(
            "evalsid is not installed. Build it with `go build ./cmd/evalsid` and put it on "
            "PATH, or set EVALSID to its path."
        )
    passthrough = [a for a in args.args if a != "--"] if args.args[:1] == ["--"] else args.args
    os.execv(binary, [binary, "serve", *passthrough])
    return 0  # pragma: no cover - execv does not return


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
                        "on_by_default": p.on_by_default,
                        "evaluators": [
                            {
                                "name": d.spec.name,
                                "version": d.spec.version,
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
        default = " (on by default)" if pack.on_by_default else ""
        print(f"{pack.name}{default}: {pack.description}")
        for d in pack.evaluators:
            params = ", ".join(k if v is _EMPTY else f"{k}={v!r}" for k, v in d.spec.params.items())
            print(f"  {d.spec.name}@{d.spec.version}  {d.spec.description}")
            if params:
                print(f"      params: {params}")
        print()
    return 0


if __name__ == "__main__":  # pragma: no cover
    sys.exit(main())
