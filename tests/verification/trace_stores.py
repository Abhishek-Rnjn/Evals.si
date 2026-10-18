"""Seed synthetic traces and independently read persisted feedback from free trace stores.

Run with the matching pinned server SDK installed. Projects/experiments should
belong to a disposable local server. This never invokes a model.
"""

from __future__ import annotations

import argparse
import json
import time
import urllib.parse
import urllib.request
import uuid


def phoenix(args):
    if args.action == "seed":
        from opentelemetry.proto.collector.trace.v1.trace_service_pb2 import (
            ExportTraceServiceRequest,
        )

        request = ExportTraceServiceRequest()
        resource = request.resource_spans.add()
        item = resource.resource.attributes.add(key="openinference.project.name")
        item.value.string_value = args.project
        span = resource.scope_spans.add().spans.add(
            trace_id=uuid.uuid4().bytes,
            span_id=uuid.uuid4().bytes[:8],
            name="verification-agent",
            start_time_unix_nano=time.time_ns() - 1_000_000_000,
            end_time_unix_nano=time.time_ns(),
        )
        for key, value in (
            ("openinference.span.kind", "AGENT"),
            ("input.value", "synthetic question"),
            ("output.value", "synthetic answer"),
        ):
            span.attributes.add(key=key).value.string_value = value
        req = urllib.request.Request(
            args.endpoint.rstrip("/") + "/v1/traces",
            data=request.SerializeToString(),
            headers={"Content-Type": "application/x-protobuf"},
        )
        with urllib.request.urlopen(req, timeout=15) as response:
            if response.status != 200:
                raise ValueError("OTLP upload failed")
        return {
            "store": "phoenix",
            "project": args.project,
            "trace_id": span.trace_id.hex(),
        }
    query = urllib.parse.urlencode(
        {
            "trace_ids": args.trace_id,
            "identifier": "evalsi/live-test",
            "include_annotation_names": "live-test",
        }
    )
    path = (
        "/v1/projects/"
        + urllib.parse.quote(args.project, safe="")
        + "/trace_annotations?"
        + query
    )
    with urllib.request.urlopen(
        args.endpoint.rstrip("/") + path, timeout=15
    ) as response:
        rows = json.load(response)["data"]
    if len(rows) != 1 or rows[0]["result"]["score"] != 0:
        raise ValueError("Phoenix did not persist exactly one latest score of zero")
    return {"store": "phoenix", "persisted_rows": 1, "latest_score": 0, "passed": True}


def mlflow(args):
    import mlflow as sdk

    sdk.set_tracking_uri(args.endpoint)
    if args.action == "seed":
        experiment = sdk.set_experiment(args.project)
        with sdk.start_span(name="verification-agent", span_type="AGENT") as span:
            span.set_inputs({"question": "synthetic question"})
            span.set_outputs("synthetic answer")
            trace_id = span.trace_id
        sdk.flush_trace_async_logging()
        return {
            "store": "mlflow",
            "project": args.project,
            "experiment_id": experiment.experiment_id,
            "trace_id": trace_id,
        }
    trace = sdk.get_trace(args.trace_id)
    rows = [
        a
        for a in trace.info.assessments
        if a.name == "live-test" and a.source.source_id == "evalsi/live-test"
    ]
    if len(rows) != 1 or rows[0].feedback.value != 0:
        raise ValueError("MLflow did not persist exactly one latest score of zero")
    return {"store": "mlflow", "persisted_rows": 1, "latest_score": 0, "passed": True}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("store", choices=("phoenix", "mlflow"))
    parser.add_argument("action", choices=("seed", "read-back"))
    parser.add_argument("--endpoint", required=True)
    parser.add_argument("--project", required=True)
    parser.add_argument("--trace-id")
    args = parser.parse_args()
    if args.action == "read-back" and not args.trace_id:
        parser.error("read-back requires --trace-id")
    print(json.dumps((phoenix if args.store == "phoenix" else mlflow)(args), indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
