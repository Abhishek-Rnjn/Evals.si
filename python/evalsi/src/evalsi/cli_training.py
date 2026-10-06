"""``evalsi checkpoints``: evaluate a model's checkpoints as it trains.

``eval`` evaluates one checkpoint (or the base model, ``--step base``);
``watch`` evaluates every new checkpoint a trainer writes to a directory
(or an ``s3://``/``hf://`` location, with fsspec); ``curve`` prints the
learning curve against the base model and exits non-zero when the latest
checkpoint regressed, so it can gate a pipeline.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import time
from pathlib import Path
from typing import Any

from evalsi.cli import EXIT_GATES_FAILED, add_credential_args


def add_training_commands(sub: Any) -> None:
    ck = sub.add_parser(
        "checkpoints", help="evaluate training checkpoints and their learning curve"
    )
    ck_sub = ck.add_subparsers(dest="action", required=True)

    ev = ck_sub.add_parser("eval", help="evaluate one checkpoint (or --step base)")
    ev.add_argument("checkpoint", help="checkpoint path, or the base model's path or name")
    ev.add_argument("--step", required=True, help="training step, or 'base'")
    _common(ev, serving=True)
    ev.set_defaults(func=_cmd_eval)

    wa = ck_sub.add_parser("watch", help="evaluate every new checkpoint in a location")
    wa.add_argument(
        "location",
        help="the trainer's output directory (s3://, hf://... with fsspec), or mlflow:<model> "
        "for a registered model's versions (MLFLOW_TRACKING_URI)",
    )
    wa.add_argument(
        "--stop-file",
        help="write this file when a checkpoint regresses; the trainer callback's stop_file "
        "(or any training loop) can watch it to stop training",
    )
    wa.add_argument("--base", help="evaluate this base model first, if not done yet")
    wa.add_argument("--interval", type=float, default=60.0, help="seconds between scans")
    wa.add_argument(
        "--settle", type=float, default=30.0, help="seconds a checkpoint must be unchanged"
    )
    wa.add_argument("--cache-dir", default="", help="where remote checkpoints are downloaded")
    wa.add_argument("--once", action="store_true", help="scan once and exit")
    _common(wa, serving=True)
    wa.set_defaults(func=_cmd_watch)

    cu = ck_sub.add_parser("curve", help="print the learning curve against the base model")
    cu.add_argument("--metrics", default="", help="comma-separated metrics to show (default: all)")
    cu.add_argument("--format", choices=["table", "json"], default="table")
    _common(cu, serving=False)
    cu.set_defaults(func=_cmd_curve)


def _common(p: argparse.ArgumentParser, *, serving: bool) -> None:
    p.add_argument(
        "-f", "--file", required=True, help="the run spec to run against each checkpoint"
    )
    p.add_argument("--training-run", required=True, help="names this training run's curve")
    p.add_argument(
        "--log-dir", default="evalsi-training", help="embedded results (default: evalsi-training)"
    )
    p.add_argument("--server", help="run on this evalsid; the history lives with its runs")
    p.add_argument("--project", default="")
    p.add_argument(
        "--regression",
        action="append",
        default=[],
        help="metric[:max_drop]: fail when significantly below the base model (repeatable)",
    )
    if serving:
        s = p.add_argument_group("serving the checkpoint")
        s.add_argument("--serve", choices=["vllm", "lora", "endpoint"], default="vllm")
        s.add_argument(
            "--base-url", help="lora: the running vLLM; endpoint: the served model's URL"
        )
        s.add_argument("--model", help="endpoint: the served model name")
        s.add_argument("--api-key-env", default="", help="lora: env var with the vLLM API key")
        s.add_argument(
            "--vllm-arg", action="append", default=[], help="extra `vllm serve` argument"
        )
    add_credential_args(p)


def _serving(args: argparse.Namespace) -> Any:
    from evalsi.training import VLLM, Endpoint, LoRA

    if args.serve == "lora":
        if not args.base_url:
            raise ValueError("--serve lora needs --base-url (the running vLLM)")
        return LoRA(args.base_url, api_key_env=args.api_key_env)
    if args.serve == "endpoint":
        if not args.base_url or not args.model:
            raise ValueError("--serve endpoint needs --base-url and --model")
        return Endpoint(args.base_url, args.model)
    return VLLM(args=args.vllm_arg, log_dir=args.log_dir)


def _evaluator(args: argparse.Namespace) -> Any:
    from evalsi.training import CheckpointEvaluator, Endpoint

    return CheckpointEvaluator(
        args.file,
        training_run=args.training_run,
        serving=_serving(args) if hasattr(args, "serve") else Endpoint("", ""),
        log_dir=args.log_dir,
        server=args.server,
        project=args.project,
        token=args.token,
        api_key=args.api_key,
        regression=args.regression,
    )


def _report(result: Any) -> int:
    from evalsi.training import format_curve

    print(format_curve([result]))
    if result.error:
        print(f"error: {result.error}", file=sys.stderr)
        return 1
    return 0 if result.passed else EXIT_GATES_FAILED


def _cmd_eval(args: argparse.Namespace) -> int:
    return _report(_evaluator(args).evaluate(args.checkpoint, args.step))


def _cmd_watch(args: argparse.Namespace) -> int:
    from evalsi.training import BASE, find
    from evalsi.training.checkpoints import MLFLOW_PREFIX, find_mlflow, localize, localize_mlflow

    evaluator = _evaluator(args)
    cache = args.cache_dir or f"{args.log_dir}/{args.training_run}/downloads"
    if args.base and evaluator.get(BASE) is None:
        print(f"evaluating the base model {args.base}", file=sys.stderr)
        _report(evaluator.evaluate_base(args.base))
    done = {r.step for r in evaluator.history() if not r.error}
    mlflow_model = (
        args.location.removeprefix("mlflow:") if args.location.startswith("mlflow:") else ""
    )
    tracking = os.environ.get("MLFLOW_TRACKING_URI", "")
    if mlflow_model and not tracking:
        raise ValueError("mlflow:<model> needs MLFLOW_TRACKING_URI")
    while True:
        found = (
            find_mlflow(mlflow_model, tracking)
            if mlflow_model
            else find(args.location, settle_s=args.settle)
        )
        for checkpoint in found:
            if str(checkpoint.step) in done:
                continue
            print(f"evaluating step {checkpoint.step}: {checkpoint.location}", file=sys.stderr)
            if checkpoint.location.startswith(MLFLOW_PREFIX):
                path = localize_mlflow(checkpoint, tracking, cache)
            else:
                path = localize(checkpoint, cache)
            result = evaluator.evaluate(path, checkpoint.step)
            _report(result)
            done.add(str(checkpoint.step))
            if args.stop_file and result.regressed:
                Path(args.stop_file).write_text(
                    json.dumps(
                        {
                            "step": result.step,
                            "regressed": [c.metric for c in result.comparisons if c.regressed],
                        }
                    )
                )
                print(f"step {result.step} regressed; wrote {args.stop_file}", file=sys.stderr)
        if args.once:
            return 0
        time.sleep(args.interval)


def _cmd_curve(args: argparse.Namespace) -> int:
    from evalsi.training import format_curve

    history = _evaluator(args).curve()
    metrics = [m for m in args.metrics.split(",") if m]
    if args.format == "json":
        print(json.dumps([r.to_dict() | {"values": None} for r in history], indent=2, default=str))
    else:
        print(format_curve(history, metrics))
    steps = [r for r in history if r.step != "base"]
    if steps and not steps[-1].passed:
        return EXIT_GATES_FAILED
    return 0
