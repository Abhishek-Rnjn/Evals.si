"""Follow an executor's event log while it runs and send it to the ingest as one trace.

A CLI agent's structured output (``codex exec --json``, ``opencode run --format
json``, and the other formats in ``formats.py``) is read by a reader only after the
process exits. ``follow`` tails the file the agent is writing instead: each poll
re-reads the new complete lines, runs the same reader over what has arrived so far, and
exports the steps that can no longer change as OTLP spans of a single trace (a fixed
trace id, a root ``invoke_agent`` span sent once, at the end). Evals.si's ingest then
scores them through the online-evaluation policies the server already runs, and writes
the scores back through whichever sinks (MLflow, Phoenix, Langfuse) the project has
configured; nothing here scores or writes scores itself.

Which steps are final: a reader builds a model-call step from several events and fills
a tool step's result when it arrives, so everything from the *latest* model-call step
onward is held back until the log ends or a newer model call starts. A long final
answer therefore appears when the run finishes; tool calls and earlier turns appear as
the run goes. The endpoint and headers are the OTLP environment variables
(``OTEL_EXPORTER_OTLP_ENDPOINT``, ``OTEL_EXPORTER_OTLP_HEADERS``, as in ``otel.py``).

The log is read as text, so a half-written last line waits for its newline. The agent's
own telemetry (spans an executor might emit itself) is not read: only its event log is.
"""

from __future__ import annotations

import asyncio
import secrets
import time
from collections.abc import Callable
from dataclasses import dataclass, field
from pathlib import Path

from evalsi.types import Step
from evalsi_harness import otel
from evalsi_harness.formats import FORMATS, ParsedRun, parse_output

MAX_LOG_BYTES = 256 << 20


class FollowError(RuntimeError):
    """The log cannot be followed (unknown format, unreadable file, nothing to send to)."""


@dataclass
class Follower:
    """The state of one followed run. ``feed`` takes log text, ``flush`` sends what is final."""

    fmt: str
    task_id: str
    trial: int = 0
    run_id: str = ""
    agent: str = "agent"
    trace_id: str = field(default_factory=lambda: secrets.token_hex(16))
    root_span_id: str = field(default_factory=lambda: secrets.token_hex(8))
    started_ns: int = field(default_factory=time.time_ns)
    sent: int = 0
    sent_ns: int = 0  # how far along the trace the sent steps reach
    text: str = ""
    _partial: str = ""
    errors: list[str] = field(default_factory=list)

    def __post_init__(self) -> None:
        if self.fmt in ("", "dsh-json") or self.fmt not in FORMATS:
            usable = [f for f in FORMATS if f]
            raise FollowError(f"--format must be one of {', '.join(usable)}")

    def feed(self, chunk: str) -> None:
        """Append log text; only complete lines count."""
        data = self._partial + chunk
        head, _, self._partial = data.rpartition("\n")
        if head or data.endswith("\n"):
            self.text += head + "\n"

    def parsed(self) -> ParsedRun:
        run = parse_output(self.fmt, self.text)
        assert run is not None
        return run

    def final_steps(self, run: ParsedRun, *, ended: bool) -> list[Step]:
        if ended:
            return run.steps
        last_llm = max((i for i, s in enumerate(run.steps) if s.type == "llm"), default=0)
        return run.steps[:last_llm]

    async def flush(self, *, ended: bool = False) -> int:
        """Send the steps that are final (all of them when ``ended``, with the root span).
        Returns how many steps went out; a failed send leaves them to the next flush."""
        if ended and self._partial.strip():
            self.feed("\n")
        run = self.parsed()
        if run.wrong_format and ended and not run.steps:
            raise FollowError(run.error)
        steps = self.final_steps(run, ended=ended)[self.sent :]
        if not steps and not ended:
            return 0
        now = time.time_ns()
        _, span_list = otel.spans(
            steps,
            task_id=self.task_id,
            trial=self.trial,
            run_id=self.run_id,
            agent=self.agent,
            started_ns=self.started_ns + self.sent_ns,
            ended_ns=now,
            trace_id=self.trace_id,
            root_span_id=self.root_span_id,
            include_root=False,
        )
        if ended:
            _, root = otel.spans(
                [],
                task_id=self.task_id,
                trial=self.trial,
                run_id=self.run_id,
                agent=self.agent,
                started_ns=self.started_ns,
                ended_ns=now,
                trace_id=self.trace_id,
                root_span_id=self.root_span_id,
            )
            span_list += root
        if not await otel.post(span_list):
            self.errors.append("export failed or no OTLP endpoint is set")
            if ended:
                raise FollowError(self.errors[-1])
            return 0
        self.sent += len(steps)
        self.sent_ns += sum(int((s.duration_ms or 0) * 1e6) for s in steps)
        return len(steps)


async def follow(
    path: str | Path,
    follower: Follower,
    *,
    poll_s: float = 1.0,
    idle_s: float = 30.0,
    on_flush: Callable[[int], None] | None = None,
) -> Follower:
    """Tail ``path`` until it has not grown for ``idle_s`` seconds (0: read what is there
    and finish), then send the rest and the root span. The file may not exist yet."""
    log = Path(path)
    offset = 0
    quiet = 0.0
    carry = b""
    while True:
        grew = False
        if log.is_file():
            size = log.stat().st_size
            if size < offset:
                raise FollowError(f"{log} was truncated while it was followed")
            if offset + MAX_LOG_BYTES < size:
                raise FollowError(f"{log} is over {MAX_LOG_BYTES} bytes")
            if size > offset:
                with log.open("rb") as handle:
                    handle.seek(offset)
                    raw = carry + handle.read(size - offset)
                offset = size
                # A multi-byte character split across reads waits for its rest.
                cut = len(raw)
                while cut and raw[cut - 1] & 0xC0 == 0x80:
                    cut -= 1
                if cut and raw[cut - 1] >= 0xC0:
                    cut -= 1
                carry = raw[cut:]
                follower.feed(raw[:cut].decode("utf-8", errors="replace"))
                grew = True
        if grew:
            quiet = 0.0
            sent = await follower.flush()
            if sent and on_flush is not None:
                on_flush(sent)
        else:
            if quiet >= idle_s:
                break
            quiet += poll_s
            await asyncio.sleep(poll_s)
    sent = await follower.flush(ended=True)
    if on_flush is not None:
        on_flush(sent)
    return follower
