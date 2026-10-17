"""Synthetic OTLP credential redaction across real NATS, PostgreSQL and the API."""

from __future__ import annotations

import argparse
import asyncio
import base64
import json
import secrets
import subprocess
import time
from pathlib import Path
from typing import Any

import httpx
import nats

from evalsi.client import Client, ServerError

NS = "evalsi-leftovers-restore"


async def verify() -> dict[str, Any]:
    credential = subprocess.check_output(
        ["kubectl", "-n", NS, "create", "token", "verify", "--audience=evals.si"], text=True
    ).strip()
    trace = secrets.token_hex(16)
    sentinel = "synthetic-redaction-" + secrets.token_hex(8)
    now = time.time_ns()

    def attribute(key: str, value: str) -> dict[str, Any]:
        return {"key": key, "value": {"stringValue": value}}

    payload = {
        "resourceSpans": [
            {
                "resource": {
                    "attributes": [
                        attribute("service.name", "redaction-proof"),
                        attribute("evalsi.project", "default"),
                        attribute("password", sentinel),
                    ]
                },
                "scopeSpans": [
                    {
                        "spans": [
                            {
                                "traceId": trace,
                                "spanId": secrets.token_hex(8),
                                "name": "invoke_agent proof",
                                "startTimeUnixNano": str(now),
                                "endTimeUnixNano": str(now + 1000000),
                                "attributes": [
                                    attribute("gen_ai.operation.name", "invoke_agent"),
                                    attribute("api_key", sentinel),
                                    attribute(
                                        "gen_ai.input.messages",
                                        json.dumps(
                                            [
                                                {
                                                    "role": "user",
                                                    "parts": [
                                                        {
                                                            "type": "text",
                                                            "content": json.dumps(
                                                                {"api_key": sentinel}
                                                            ),
                                                        }
                                                    ],
                                                }
                                            ]
                                        ),
                                    ),
                                    attribute(
                                        "gen_ai.output.messages",
                                        json.dumps(
                                            [
                                                {
                                                    "role": "assistant",
                                                    "parts": [{"type": "text", "content": "done"}],
                                                }
                                            ]
                                        ),
                                    ),
                                ],
                            }
                        ]
                    }
                ],
            }
        ]
    }
    response = httpx.post(
        "http://localhost:19118/v1/traces",
        json=payload,
        headers={"Authorization": "Bearer " + credential},
    )
    response.raise_for_status()
    with Client("http://localhost:18080", token=credential) as client:
        for _ in range(100):
            try:
                saved = client.call(
                    "TraceService", "GetTrace", {"traceId": trace, "project": "default"}
                )
                break
            except ServerError as exc:
                if exc.code != "not_found":
                    raise
                await asyncio.sleep(0.2)
        else:
            raise AssertionError("trace never became queryable")
    rendered = json.dumps(saved)
    assert sentinel not in rendered
    assert "[REDACTED]" in rendered
    sql = (
        f"SELECT count(*) FROM traces WHERE trace_id='{trace}' "
        f"AND position(convert_to('{sentinel}','UTF8') in record)=0 "
        "AND position(convert_to('[REDACTED]','UTF8') in record)>0"
    )
    found = subprocess.check_output(
        [
            "kubectl",
            "-n",
            NS,
            "exec",
            "restored-dev-postgres-0",
            "--",
            "psql",
            "-U",
            "evalsi",
            "-d",
            "evalsi",
            "-Atc",
            sql,
        ],
        text=True,
    ).strip()
    assert found == "1", found
    secret = json.loads(
        subprocess.check_output(
            ["kubectl", "-n", NS, "get", "secret", "restored-nats-auth", "-o", "json"]
        )
    )["data"]["token"]
    nc = await nats.connect("nats://localhost:14222", token=base64.b64decode(secret).decode())
    try:
        js = nc.jetstream()
        info = await js.stream_info("EVALSI_SPANS")
        matching = []
        for sequence in range(info.state.first_seq, info.state.last_seq + 1):
            message = await js.get_msg("EVALSI_SPANS", seq=sequence)
            if trace.encode() in message.data or bytes.fromhex(trace) in message.data:
                matching.append(message.data)
        assert matching, "no retained broker message for this trace"
        assert all(sentinel.encode() not in data for data in matching)
        assert any(b"[REDACTED]" in data for data in matching)
    finally:
        await nc.close()
    return {
        "input": "synthetic OTLP credential fields and JSON input",
        "broker_messages_checked": len(matching),
        "raw_sentinel": "absent from NATS, PostgreSQL record and authenticated API",
        "replacement": "present in all three",
        "scope": "push ingestion; pull and ClickHouse not exercised",
    }


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--report", type=Path, required=True)
    args = parser.parse_args()
    result = asyncio.run(verify())
    args.report.write_text(json.dumps(result, indent=2) + "\n")
    print("PASS " + json.dumps(result))
