"""Evals.si rewards in OpenRLHF.

Two ways in:

- **A reward function file.** ``--remote_rm_url`` accepts a Python file
  defining ``reward_func(queries, prompts, labels)``; pass this file and set
  ``EVALSI_REWARD_SPEC`` (and optionally ``EVALSI_REWARD_SERVER``).
- **A remote reward model.** ``evalsi rewards serve --spec reward.yaml``
  speaks OpenRLHF's remote reward-model protocol over HTTP; pass its URL as
  ``--remote_rm_url``.

OpenRLHF sends whole sequences (``queries``, prompt plus response) with the
prompts and labels; the completion is the query after its prompt, and the
label is the reference answer.
"""

from __future__ import annotations

import json
import logging
from collections.abc import Sequence
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any

from evalsi.rewards import Reward, RewardResult
from evalsi.rewards.verl import get_reward

logger = logging.getLogger(__name__)


def completion_of(query: str, prompt: str) -> str:
    return query[len(prompt) :] if prompt and query.startswith(prompt) else query


def score_queries(
    reward: Reward,
    queries: Sequence[str],
    prompts: Sequence[str] | None = None,
    labels: Sequence[Any] | None = None,
) -> list[RewardResult]:
    prompts = list(prompts) if prompts is not None else [""] * len(queries)
    if len(prompts) != len(queries):
        raise ValueError(f"{len(queries)} queries but {len(prompts)} prompts")
    if labels is not None and len(labels) != len(queries):
        raise ValueError(f"{len(queries)} queries but {len(labels)} labels")
    rollouts: list[dict[str, Any]] = []
    for i, (query, prompt) in enumerate(zip(queries, prompts, strict=True)):
        rollout: dict[str, Any] = {"prompt": prompt, "completion": completion_of(query, prompt)}
        if labels is not None and labels[i] is not None:
            rollout["label"] = labels[i]
        rollouts.append(rollout)
    return reward.score(rollouts)


def response(results: Sequence[RewardResult]) -> dict[str, Any]:
    """OpenRLHF's reward payload: ``rewards`` (trained on), ``scores`` (the
    same, for dynamic filtering) and ``extra_logs`` per component."""
    totals = [r.total if r.total is not None else 0.0 for r in results]
    logs: dict[str, list[float]] = {}
    for r in results:
        for name, c in r.components.items():
            logs.setdefault(name, []).append(c.value if c.value is not None else 0.0)
    return {"rewards": totals, "scores": totals, "extra_logs": logs}


def reward_func(queries: Sequence[str], prompts: Sequence[str], labels: Sequence[Any]) -> Any:
    """The function OpenRLHF calls when ``--remote_rm_url`` is this file."""
    import torch

    payload = response(score_queries(get_reward(), queries, prompts, labels))
    return {
        "rewards": torch.tensor(payload["rewards"], dtype=torch.float32),
        "scores": torch.tensor(payload["scores"], dtype=torch.float32),
        "extra_logs": {
            k: torch.tensor(v, dtype=torch.float32) for k, v in payload["extra_logs"].items()
        },
    }


def make_server(reward: Reward, host: str = "127.0.0.1", port: int = 5000) -> ThreadingHTTPServer:
    """An HTTP server speaking OpenRLHF's remote reward-model protocol: POST
    ``{"query": [...], "prompts": [...], "labels": [...]}`` to any path."""

    class Handler(BaseHTTPRequestHandler):
        def do_POST(self) -> None:
            try:
                length = int(self.headers.get("Content-Length") or 0)
                body = json.loads(self.rfile.read(length) or b"{}")
                queries = body.get("query", body.get("queries"))
                if not isinstance(queries, list):
                    raise ValueError("the body needs a 'query' list")
                results = score_queries(reward, queries, body.get("prompts"), body.get("labels"))
                status, payload = 200, response(results)
            except Exception as exc:
                logger.exception("scoring failed")
                status, payload = 500, {"error": f"{type(exc).__name__}: {exc}"}
            data = json.dumps(payload).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def do_GET(self) -> None:
            data = b"ok\n"
            self.send_response(200)
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def log_message(self, format: str, *args: Any) -> None:
            logger.debug(format, *args)

    return ThreadingHTTPServer((host, port), Handler)
