"""``evalsi rewards``: score rollouts with a RewardSpec, or serve it to a trainer.

``score`` reads rollouts (JSONL: prompt, completion, reference columns and
anything the verifiers need) and prints one JSON line per rollout with its
total and breakdown, plus a summary on stderr. ``serve`` exposes the reward
over OpenRLHF's remote reward-model protocol.
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any

from evalsi.cli import add_credential_args


def add_rewards_commands(sub: Any) -> None:
    rw = sub.add_parser("rewards", help="score rollouts with a RewardSpec, or serve it to trainers")
    rw_sub = rw.add_subparsers(dest="action", required=True)

    sc = rw_sub.add_parser("score", help="score rollouts from a JSONL file")
    sc.add_argument("--spec", required=True, help="the RewardSpec (YAML or JSON)")
    sc.add_argument("--data", required=True, help="rollouts, one JSON object per line")
    sc.add_argument("--server", help="score on this evalsid's Reward Service (default: here)")
    sc.add_argument("--project", default="", help="the server project")
    sc.add_argument("--concurrency", type=int)
    add_credential_args(sc)
    sc.set_defaults(func=_cmd_score)

    sv = rw_sub.add_parser(
        "serve", help="serve the reward over OpenRLHF's remote reward-model protocol"
    )
    sv.add_argument("--spec", required=True, help="the RewardSpec (YAML or JSON)")
    sv.add_argument("--host", default="127.0.0.1")
    sv.add_argument("--port", type=int, default=5000)
    sv.add_argument("--server", help="score on this evalsid's Reward Service (default: here)")
    sv.add_argument("--project", default="", help="the server project")
    add_credential_args(sv)
    sv.set_defaults(func=_cmd_serve)


def _load(args: argparse.Namespace) -> Any:
    from evalsi.rewards import load

    if args.server:
        return load(
            args.spec,
            server=args.server,
            project=args.project,
            token=args.token,
            api_key=args.api_key,
            concurrency=getattr(args, "concurrency", None),
        )
    return load(args.spec, concurrency=getattr(args, "concurrency", None))


def _cmd_score(args: argparse.Namespace) -> int:
    rollouts = []
    for n, line in enumerate(Path(args.data).read_text().splitlines(), 1):
        if line.strip():
            try:
                rollouts.append(json.loads(line))
            except json.JSONDecodeError as exc:
                raise ValueError(f"{args.data}:{n}: {exc}") from exc
    reward = _load(args)
    results = reward.score(rollouts)
    for rollout, result in zip(rollouts, results, strict=True):
        out = {"id": rollout.get("id"), **result.to_dict()} if "id" in rollout else result.to_dict()
        print(json.dumps(out))
    totals = [r.total for r in results if r.total is not None]
    mean = sum(totals) / len(totals) if totals else float("nan")
    gated = sum(r.gated for r in results)
    print(
        f"{len(results)} rollouts, mean reward {mean:.4f}, {gated} gated",
        file=sys.stderr,
    )
    return 0


def _cmd_serve(args: argparse.Namespace) -> int:
    from evalsi.rewards.openrlhf import make_server

    reward = _load(args)
    server = make_server(reward, args.host, args.port)
    port = server.server_address[1]
    print(
        f"serving reward {reward.spec.name!r} on http://{args.host}:{port}",
        file=sys.stderr,
        flush=True,
    )
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()
    return 0
