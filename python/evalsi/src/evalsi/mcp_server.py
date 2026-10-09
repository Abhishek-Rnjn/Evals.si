"""``evalsi mcp``: Evals.si as an MCP server over stdio.

A coding agent (or any MCP client) can check its own work:

- ``list_evaluators``: the installed evaluators and their params;
- ``evaluate``: score records it already has, in-process;
- ``run``: execute a run spec (a suite in the repository), save the results,
  and compare them with the previous results of the same suite;
- ``compare``: paired comparison of two results (files, or server runs).

Runs execute embedded, or on an evalsid server with ``--server``. Files are
read and written only inside the workspace root (the working directory by
default), whatever path a client sends.

The transport is MCP's stdio transport: newline-delimited JSON-RPC 2.0 on
stdin and stdout. stdout carries only protocol messages; anything else the
process prints goes to stderr. Requests are handled concurrently, so ``ping``
and cancellation work during a long run, and a request that carries a
``progressToken`` gets ``notifications/progress``.
"""

from __future__ import annotations

import asyncio
import contextlib
import json
import os
import re
import sys
from collections.abc import Awaitable, Callable, Mapping, Sequence
from dataclasses import dataclass, field
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

from evalsi._version import __version__

PROTOCOL_VERSIONS = ("2025-06-18", "2025-03-26", "2024-11-05")
MAX_RECORDS = 10_000
# Per-record rows in a tool result; the full results are in the saved file.
MAX_ROWS = 50

PARSE_ERROR, INVALID_REQUEST, METHOD_NOT_FOUND, INVALID_PARAMS, INTERNAL_ERROR = (
    -32700,
    -32600,
    -32601,
    -32602,
    -32603,
)


class ToolError(Exception):
    """A tool failed in a way the client should see (isError: true)."""


class RPCError(Exception):
    def __init__(self, code: int, message: str) -> None:
        super().__init__(message)
        self.code = code


Notify = Callable[[str, dict[str, Any]], Awaitable[None]]


@dataclass
class Tool:
    name: str
    description: str
    input_schema: dict[str, Any]
    handler: Callable[
        [dict[str, Any], Callable[[float, float, str], Awaitable[None]]], Awaitable[Any]
    ]
    read_only: bool = False

    def describe(self) -> dict[str, Any]:
        return {
            "name": self.name,
            "description": self.description,
            "inputSchema": self.input_schema,
            "annotations": {"readOnlyHint": self.read_only, "openWorldHint": False},
        }


@dataclass
class Settings:
    root: Path
    server: str | None = None
    project: str = ""
    token: str | None = None
    api_key: str | None = None
    judge: Any = None
    cache: bool = True
    concurrency: int = 8
    # Where run results are saved, relative to the root.
    results_dir: str = ".evalsi/results"
    extra: dict[str, Any] = field(default_factory=dict)


def _safe_name(name: str) -> str:
    return re.sub(r"[^A-Za-z0-9_.-]+", "-", name).strip("-.") or "run"


class EvalsiMCP:
    def __init__(self, settings: Settings) -> None:
        self.s = settings
        self.root = settings.root.resolve()
        self.tools = {t.name: t for t in self._tools()}

    # --- paths -----------------------------------------------------------

    def path(self, value: Any, what: str) -> Path:
        """A path inside the workspace root."""
        if not isinstance(value, str) or not value:
            raise ToolError(f"{what} must be a path")
        p = (self.root / value).resolve()
        if p != self.root and self.root not in p.parents:
            raise ToolError(f"{what} {value!r} is outside the workspace ({self.root})")
        return p

    def rel(self, p: Path) -> str:
        return str(p.relative_to(self.root))

    # --- tools -----------------------------------------------------------

    def _tools(self) -> list[Tool]:
        evaluator_ref = {
            "oneOf": [
                {"type": "string", "description": "an evaluator reference, e.g. exact-match"},
                {
                    "type": "object",
                    "properties": {
                        "ref": {"type": "string"},
                        "name": {"type": "string"},
                        "params": {"type": "object"},
                    },
                    "required": ["ref"],
                },
            ]
        }
        return [
            Tool(
                "list_evaluators",
                "List the installed evaluators (by pack), with their parameters.",
                {
                    "type": "object",
                    "properties": {"pack": {"type": "string", "description": "only this pack"}},
                },
                self._list_evaluators,
                read_only=True,
            ),
            Tool(
                "evaluate",
                "Score records you already have (input, output, reference) with evaluators. "
                "Returns each metric's mean and confidence interval, and per-record scores.",
                {
                    "type": "object",
                    "properties": {
                        "records": {
                            "type": "array",
                            "items": {
                                "type": "object",
                                "properties": {
                                    "id": {"type": "string"},
                                    "input": {},
                                    "output": {},
                                    "reference": {},
                                    "metadata": {"type": "object"},
                                },
                            },
                        },
                        "evaluators": {"type": "array", "items": evaluator_ref, "minItems": 1},
                    },
                    "required": ["records", "evaluators"],
                },
                self._evaluate,
                read_only=True,
            ),
            Tool(
                "run",
                "Run a run spec (run.yaml) from the workspace: embedded, or on the configured "
                "server. Saves the results and, by default, compares them with the previous "
                "results of the same run (paired by record), reporting significant changes "
                "and gates.",
                {
                    "type": "object",
                    "properties": {
                        "file": {"type": "string", "description": "the run spec, in the workspace"},
                        "baseline": {
                            "type": "string",
                            "description": "'previous' (default), 'none', a results file, or "
                            "a server run id",
                        },
                    },
                    "required": ["file"],
                },
                self._run,
            ),
            Tool(
                "compare",
                "Paired comparison of two results: results files in the workspace, or run ids "
                "on the configured server.",
                {
                    "type": "object",
                    "properties": {
                        "baseline": {"type": "string"},
                        "candidate": {"type": "string"},
                    },
                    "required": ["baseline", "candidate"],
                },
                self._compare,
                read_only=True,
            ),
        ]

    async def _list_evaluators(self, args: dict[str, Any], progress: Any) -> Any:
        from evalsi.registry import default_registry

        registry = default_registry()
        pack = args.get("pack")
        packs = [p for p in registry.packs.values() if pack in (None, "", p.name)]
        if pack and not packs:
            raise ToolError(f"no pack named {pack!r}; installed: {sorted(registry.packs)}")
        data: list[dict[str, Any]] = [
            {
                "pack": p.name,
                "on_by_default": p.on_by_default,
                "evaluators": [
                    {
                        "name": d.spec.short_name,
                        "ref": d.spec.ref,
                        "description": d.spec.description,
                        "params": sorted(d.spec.params),
                        "needs_judge": bool(d.spec.requires.judge),
                    }
                    for d in p.evaluators
                ],
            }
            for p in packs
        ]
        lines = []
        for p in data:
            lines.append(f"{p['pack']}:")
            for e in p["evaluators"]:
                extra = " (needs a judge)" if e["needs_judge"] else ""
                lines.append(f"  {e['name']}{extra}: {e['description']}")
        return "\n".join(lines), {"packs": data}

    async def _evaluate(self, args: dict[str, Any], progress: Any) -> Any:
        from evalsi.runner import aevaluate

        records = args.get("records")
        evaluators = args.get("evaluators")
        if not isinstance(records, list) or not records:
            raise ToolError("records must be a non-empty array")
        if len(records) > MAX_RECORDS:
            raise ToolError(f"at most {MAX_RECORDS} records per call")
        if not isinstance(evaluators, list) or not evaluators:
            raise ToolError("evaluators must be a non-empty array")
        refs: list[Any] = []
        for e in evaluators:
            if isinstance(e, str) or (isinstance(e, dict) and isinstance(e.get("ref"), str)):
                refs.append(e)
            else:
                raise ToolError(f"evaluator {e!r}: use a reference or {{ref, params}}")
        rows = [
            {"id": str(r.get("id") or i), **{k: v for k, v in r.items() if k != "id"}}
            for i, r in enumerate(records)
            if isinstance(r, dict)
        ]

        def on_progress(done: int, total: int) -> None:
            _schedule(progress(done, total, "evaluating"))

        try:
            result = await aevaluate(
                rows,
                refs,
                judge=self.s.judge,
                cache=self.s.cache,
                concurrency=self.s.concurrency,
                on_progress=on_progress,
            )
        except (ValueError, KeyError, TypeError) as exc:
            raise ToolError(str(exc)) from exc
        data = result.to_dict()
        text = _summary_text(data.get("summaries") or [], [])
        text += "\n\n" + _record_rows(data.get("results") or [])
        return text, {
            "summaries": data.get("summaries"),
            "results": (data.get("results") or [])[: MAX_ROWS * len(refs)],
        }

    async def _run(self, args: dict[str, Any], progress: Any) -> Any:
        from evalsi.runspec import load_spec

        path = self.path(args.get("file"), "file")
        if not path.is_file():
            raise ToolError(f"no run spec at {self.rel(path)}")
        try:
            run_file = load_spec(path)
        except (ValueError, OSError) as exc:
            raise ToolError(f"{self.rel(path)}: {exc}") from exc
        baseline = str(args.get("baseline") or "previous")
        if self.s.server:
            return await self._run_on_server(run_file, baseline, progress)
        name = _safe_name(run_file.name or path.stem)
        results_dir = self.path(self.s.results_dir, "results_dir")
        previous = self._latest(results_dir, name)
        data = await self._execute(run_file, progress)
        results_dir.mkdir(parents=True, exist_ok=True)
        stamp = datetime.now(UTC).strftime("%Y%m%dT%H%M%S%fZ")
        saved = results_dir / f"{name}-{stamp}.json"
        saved.write_text(json.dumps(data, indent=2, default=str) + "\n", encoding="utf-8")
        structured: dict[str, Any] = {
            "results_file": self.rel(saved),
            "passed": all(g.get("passed") for g in data.get("gates") or []),
            "summaries": data.get("summaries"),
            "gates": data.get("gates"),
        }
        text = f"results: {self.rel(saved)}\n" + _summary_text(
            data.get("summaries") or [], data.get("gates") or []
        )
        base_path: Path | None = None
        if baseline == "previous":
            base_path = previous
        elif baseline != "none":
            base_path = self.path(baseline, "baseline")
        if base_path is not None:
            base = _load_results(base_path, self.rel(base_path))
            comparisons = _compare(base, data)
            structured["baseline"] = self.rel(base_path)
            structured["comparisons"] = comparisons
            text += f"\n\ncompared with {self.rel(base_path)}:\n" + _comparison_text(comparisons)
        elif baseline == "previous":
            text += "\n\n(no previous results of this run to compare with)"
        return text, structured

    def _latest(self, results_dir: Path, name: str) -> Path | None:
        if not results_dir.is_dir():
            return None
        pattern = re.compile(re.escape(name) + r"-\d{8}T\d{12}Z\.json")
        found = sorted(p for p in results_dir.iterdir() if pattern.fullmatch(p.name))
        return found[-1] if found else None

    async def _execute(self, run_file: Any, progress: Any) -> dict[str, Any]:
        from evalsi.run import BudgetExceeded, execute

        def on_progress(stage: str, done: int, total: int) -> None:
            _schedule(progress(done, total, stage))

        try:
            outcome = await execute(
                run_file,
                judge=self.s.judge,
                cache=self.s.cache,
                concurrency=self.s.concurrency,
                on_progress=on_progress,
            )
        except BudgetExceeded as exc:
            raise ToolError(f"budget exceeded: {exc}") from exc
        except (ValueError, KeyError, TypeError, OSError) as exc:
            raise ToolError(str(exc)) from exc
        data = outcome.result.to_dict()
        data["gates"] = [g.to_dict() for g in outcome.gates]
        return data

    def _client(self) -> Any:
        from evalsi.client import Client

        assert self.s.server
        return Client(self.s.server, token=self.s.token, api_key=self.s.api_key)

    async def _run_on_server(self, run_file: Any, baseline: str, progress: Any) -> Any:
        from evalsi.client import ServerError
        from evalsi.runspec import spec_to_dict

        loop = asyncio.get_running_loop()

        def work() -> tuple[dict[str, Any], str | None, list[dict[str, Any]]]:
            with self._client() as client:
                base_id: str | None = None
                if baseline == "previous":
                    base_id = _previous_server_run(
                        client, run_file.name, run_file.project or self.s.project
                    )
                elif baseline != "none":
                    base_id = baseline
                run = client.create_run(
                    spec_to_dict(run_file.spec),
                    name=run_file.name,
                    project=run_file.project or self.s.project,
                    labels=run_file.labels,
                )
                for event in client.watch_run(run["id"]):
                    if "run" in event:
                        run = event["run"]
                    elif "progress" in event:
                        p = event["progress"]
                        asyncio.run_coroutine_threadsafe(
                            progress(p.get("done", 0), p.get("total", 0), run["id"]), loop
                        )
                comparisons = client.compare_runs(base_id, run["id"]) if base_id else []
                return run, base_id, comparisons

        try:
            run, base_id, comparisons = await asyncio.to_thread(work)
        except ServerError as exc:
            raise ToolError(str(exc)) from exc
        status = str(run.get("status", "")).removeprefix("RUN_STATUS_").lower()
        gates = [
            {**(g.get("gate") or {}), "passed": bool(g.get("passed")), "value": g.get("value")}
            for g in run.get("gates", [])
        ]
        text = f"run {run.get('id')} on {self.s.server}: {status}\n" + _summary_text(
            run.get("summaries") or [], gates
        )
        if run.get("error"):
            text += f"\nerror: {run['error']}"
        structured: dict[str, Any] = {
            "run_id": run.get("id"),
            "status": status,
            "summaries": run.get("summaries"),
            "gates": gates,
        }
        if base_id:
            converted = [_from_server_comparison(c) for c in comparisons]
            structured["baseline"] = base_id
            structured["comparisons"] = converted
            text += f"\n\ncompared with run {base_id}:\n" + _comparison_text(converted)
        return text, structured

    async def _compare(self, args: dict[str, Any], progress: Any) -> Any:
        baseline, candidate = args.get("baseline"), args.get("candidate")
        if not isinstance(baseline, str) or not isinstance(candidate, str):
            raise ToolError("baseline and candidate are required")
        if self.s.server and not baseline.endswith(".json"):

            def work() -> list[dict[str, Any]]:
                with self._client() as client:
                    out: list[dict[str, Any]] = client.compare_runs(baseline, candidate)
                    return out

            from evalsi.client import ServerError

            try:
                comparisons = [_from_server_comparison(c) for c in await asyncio.to_thread(work)]
            except ServerError as exc:
                raise ToolError(str(exc)) from exc
        else:
            a = self.path(baseline, "baseline")
            b = self.path(candidate, "candidate")
            comparisons = _compare(_load_results(a, baseline), _load_results(b, candidate))
        return _comparison_text(comparisons), {"comparisons": comparisons}

    # --- protocol ----------------------------------------------------------

    async def handle(self, message: Any, notify: Notify) -> dict[str, Any] | None:
        """One JSON-RPC message; the response, or None for a notification."""
        if not isinstance(message, dict) or message.get("jsonrpc") != "2.0":
            return _error(None, INVALID_REQUEST, "not a JSON-RPC 2.0 message")
        method = message.get("method")
        msg_id = message.get("id")
        is_request = "id" in message
        if not isinstance(method, str):
            return _error(msg_id, INVALID_REQUEST, "no method") if is_request else None
        params = message.get("params") or {}
        try:
            result = await self._dispatch(method, params, notify)
        except RPCError as exc:
            return _error(msg_id, exc.code, str(exc)) if is_request else None
        except Exception as exc:  # never let one request take the server down
            return (
                _error(msg_id, INTERNAL_ERROR, f"{type(exc).__name__}: {exc}")
                if is_request
                else None
            )
        if not is_request:
            return None
        return {"jsonrpc": "2.0", "id": msg_id, "result": result}

    async def _dispatch(self, method: str, params: Any, notify: Notify) -> Any:
        if not isinstance(params, dict):
            raise RPCError(INVALID_PARAMS, "params must be an object")
        if method == "initialize":
            asked = str(params.get("protocolVersion", ""))
            version = asked if asked in PROTOCOL_VERSIONS else PROTOCOL_VERSIONS[0]
            return {
                "protocolVersion": version,
                "capabilities": {"tools": {"listChanged": False}},
                "serverInfo": {"name": "evalsi", "title": "Evals.si", "version": __version__},
                "instructions": (
                    "Evaluate your work with Evals.si: `run` a suite from the repository "
                    "(it compares with the previous run of the same suite), `evaluate` "
                    "records directly, or `list_evaluators` to see what can score them."
                ),
            }
        if method == "ping":
            return {}
        if method.startswith("notifications/"):
            return None
        if method == "tools/list":
            return {"tools": [t.describe() for t in self.tools.values()]}
        if method == "tools/call":
            return await self._call(params, notify)
        raise RPCError(METHOD_NOT_FOUND, f"method {method!r} is not supported")

    async def _call(self, params: dict[str, Any], notify: Notify) -> dict[str, Any]:
        name = params.get("name")
        tool = self.tools.get(name) if isinstance(name, str) else None
        if tool is None:
            raise RPCError(INVALID_PARAMS, f"unknown tool {name!r}")
        args = params.get("arguments") or {}
        if not isinstance(args, dict):
            raise RPCError(INVALID_PARAMS, "arguments must be an object")
        token = (params.get("_meta") or {}).get("progressToken")

        async def progress(done: float, total: float, message: str) -> None:
            if token is None:
                return
            body: dict[str, Any] = {"progressToken": token, "progress": done, "message": message}
            if total:
                body["total"] = total
            await notify("notifications/progress", body)

        try:
            text, structured = await tool.handler(args, progress)
        except ToolError as exc:
            return {"content": [{"type": "text", "text": f"error: {exc}"}], "isError": True}
        return {
            "content": [{"type": "text", "text": text}],
            "structuredContent": json.loads(json.dumps(structured, default=str)),
            "isError": False,
        }


# --- formatting and helpers ---------------------------------------------------


def _schedule(coro: Awaitable[None]) -> None:
    with contextlib.suppress(RuntimeError):
        asyncio.get_running_loop().create_task(coro)  # type: ignore[arg-type]


def _error(msg_id: Any, code: int, message: str) -> dict[str, Any]:
    return {"jsonrpc": "2.0", "id": msg_id, "error": {"code": code, "message": message}}


def _fmt(v: Any) -> str:
    return "-" if v is None else f"{float(v):.3f}"


def _summary_text(
    summaries: Sequence[Mapping[str, Any]], gates: Sequence[Mapping[str, Any]]
) -> str:
    lines = ["metric  n  mean  [CI]"]
    for s in summaries:
        ci = s.get("ci") or {}
        interval = f"[{_fmt(ci.get('low'))}, {_fmt(ci.get('high'))}]" if ci else ""
        extra = ""
        if s.get("errors"):
            extra += f"  errors={s['errors']}"
        if s.get("skipped"):
            extra += f"  skipped={s['skipped']}"
        lines.append(
            f"{s.get('metric')}  {s.get('n', 0)}  {_fmt(s.get('mean'))}  {interval}{extra}"
        )
    for g in gates:
        verdict = "passed" if g.get("passed") else "FAILED"
        bound = " ".join(f"{k} {g[k]}" for k in ("min", "max") if g.get(k) is not None)
        lines.append(f"gate {g.get('metric')} {bound}: {verdict} ({_fmt(g.get('value'))})")
    return "\n".join(lines)


def _record_rows(results: Sequence[Mapping[str, Any]]) -> str:
    lines = []
    for r in results[:MAX_ROWS]:
        rid = r.get("record_id", "")
        outcome = r.get("outcome", "")
        if outcome != "scored":
            lines.append(f"{rid} {r.get('evaluator')}: {outcome} {r.get('reason', '')}".rstrip())
            continue
        for s in r.get("scores") or []:
            value = s.get("number", s.get("passed", s.get("label")))
            why = f" ({s['explanation']})" if s.get("explanation") else ""
            lines.append(f"{rid} {s.get('name') or r.get('evaluator')}: {value}{why}")
    if len(results) > MAX_ROWS:
        lines.append(f"... {len(results) - MAX_ROWS} more results")
    return "\n".join(lines)


def _load_results(path: Path, shown: str) -> dict[str, Any]:
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, ValueError) as exc:
        raise ToolError(f"{shown}: {exc}") from exc
    if not isinstance(data, dict) or "summaries" not in data:
        raise ToolError(f"{shown} is not a results file (evalsi run --output)")
    return data


def _compare(base: Mapping[str, Any], cand: Mapping[str, Any]) -> list[dict[str, Any]]:
    from evalsi.compare import compare_results

    return [c.to_dict() for c in compare_results(base, cand)]


def _from_server_comparison(c: Mapping[str, Any]) -> dict[str, Any]:
    ci = c.get("diffCi") or {}
    return {
        "metric": c.get("metric"),
        "baseline_mean": c.get("baselineMean"),
        "candidate_mean": c.get("candidateMean"),
        "paired_n": int(c.get("pairedN", 0) or 0),
        "diff": c.get("diff"),
        "diff_low": ci.get("low"),
        "diff_high": ci.get("high"),
        "significant": bool(c.get("significant")),
        "regressed": bool(c.get("regressed")),
    }


def _comparison_text(comparisons: Sequence[Mapping[str, Any]]) -> str:
    if not comparisons:
        return "no shared metrics"
    lines = []
    for c in comparisons:
        diff = c.get("diff")
        sign = "" if diff is None else ("+" if diff >= 0 else "")
        interval = ""
        if c.get("diff_low") is not None:
            interval = f" [{_fmt(c['diff_low'])}, {_fmt(c['diff_high'])}]"
        verdict = (
            "REGRESSED"
            if c.get("regressed")
            else "improved"
            if c.get("significant")
            else "no significant change"
        )
        lines.append(
            f"{c['metric']}: {_fmt(c.get('baseline_mean'))} -> {_fmt(c.get('candidate_mean'))} "
            f"(diff {sign}{_fmt(diff)}{interval}, n={c.get('paired_n', 0)}): {verdict}"
        )
    return "\n".join(lines)


def _previous_server_run(client: Any, name: str, project: str) -> str | None:
    if not name:
        return None
    token = ""
    for _ in range(5):
        page = client.list_runs(project=project, page_token=token)
        for run in page.get("runs", []):
            status = str(run.get("status", ""))
            if run.get("name") == name and status in ("RUN_STATUS_SUCCEEDED", "RUN_STATUS_FAILED"):
                return str(run["id"])
        token = page.get("nextPageToken", "")
        if not token:
            break
    return None


# --- stdio transport -----------------------------------------------------------


async def serve_stdio(server: EvalsiMCP) -> None:
    """Serve MCP on stdin/stdout until stdin closes."""
    # Protocol messages go to the original stdout; everything else the process
    # (or a child) writes to file descriptor 1 lands on stderr instead.
    out = os.fdopen(os.dup(1), "wb", buffering=0)
    os.dup2(2, 1)
    sys.stdout = sys.stderr
    await serve(server, *(await _stdin_reader()), out)


async def _stdin_reader() -> tuple[asyncio.StreamReader]:
    loop = asyncio.get_running_loop()
    reader = asyncio.StreamReader(limit=64 << 20)
    await loop.connect_read_pipe(lambda: asyncio.StreamReaderProtocol(reader), sys.stdin)
    return (reader,)


async def serve(server: EvalsiMCP, reader: asyncio.StreamReader, out: Any) -> None:
    lock = asyncio.Lock()
    tasks: dict[Any, asyncio.Task[None]] = {}

    async def send(message: dict[str, Any]) -> None:
        line = json.dumps(message, separators=(",", ":"), default=str).encode() + b"\n"
        async with lock:
            out.write(line)
            with contextlib.suppress(AttributeError):
                out.flush()

    async def notify(method: str, params: dict[str, Any]) -> None:
        await send({"jsonrpc": "2.0", "method": method, "params": params})

    async def one(message: Any) -> None:
        try:
            response = await server.handle(message, notify)
        except asyncio.CancelledError:
            return  # cancelled by the client: no response, as the spec says
        if response is not None:
            await send(response)

    while True:
        line = await reader.readline()
        if not line:
            break
        if not line.strip():
            continue
        try:
            message = json.loads(line)
        except ValueError:
            await send(_error(None, PARSE_ERROR, "invalid JSON"))
            continue
        if isinstance(message, dict) and message.get("method") == "notifications/cancelled":
            rid = (message.get("params") or {}).get("requestId")
            task = tasks.get(rid)
            if task is not None:
                task.cancel()
            continue
        task = asyncio.create_task(one(message))
        if isinstance(message, dict) and "id" in message:
            rid = message["id"]
            tasks[rid] = task

            def forget(_: asyncio.Task[None], rid: Any = rid) -> None:
                tasks.pop(rid, None)

            task.add_done_callback(forget)
    if tasks:
        await asyncio.gather(*tasks.values(), return_exceptions=True)
