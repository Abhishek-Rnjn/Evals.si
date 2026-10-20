"""Pinned release policies and portable decision verification.

Hashes detect changes relative to an independently trusted pin. They are not
signatures, deployment authorization or proof that a declared commit executed.
"""

from __future__ import annotations

import hashlib
import json
import os
from collections.abc import Mapping
from pathlib import Path
from typing import Any

import yaml
from google.protobuf import json_format
from google.protobuf.message import Message

from evalsi.client import Client
from evalsi.v1alpha1.release_service_pb2 import ReleaseDecision, ReleasePolicy


def _digest(domain: str, message: Message) -> str:
    return hashlib.sha256(
        domain.encode() + b"\0" + message.SerializeToString(deterministic=True)
    ).hexdigest()


def policy_hash(policy: Mapping[str, Any]) -> str:
    """Hash the exact protobuf policy; field names may use snake_case or camelCase."""
    try:
        message = json_format.ParseDict(dict(policy), ReleasePolicy())
    except json_format.ParseError as exc:
        raise ValueError(f"invalid release policy: {exc}") from exc
    return _digest("evalsi.release-policy/v1", message)


def decision_hash(decision: Mapping[str, Any]) -> str:
    try:
        message = json_format.ParseDict(dict(decision), ReleaseDecision())
    except json_format.ParseError as exc:
        raise ValueError(f"invalid release decision: {exc}") from exc
    message.ClearField("sha256")
    return _digest("evalsi.release-decision/v1", message)


def verify_decision(decision: Mapping[str, Any], expected: str) -> str:
    digest = decision_hash(decision)
    if not expected or digest != expected or decision.get("sha256") != digest:
        raise ValueError("release decision SHA-256 mismatch")
    return digest


def exit_code(decision: Mapping[str, Any]) -> int:
    """Fail closed: passing=0, blocked=3, insufficient/invalid=1."""
    return {"RELEASE_OUTCOME_PASS": 0, "RELEASE_OUTCOME_BLOCK": 3}.get(
        str(decision.get("outcome")), 1
    )


def write_report(path: str | Path, decision: dict[str, Any]) -> None:
    """Create a private report without replacing an existing path."""
    verify_decision(decision, decision.get("sha256", ""))
    raw = (
        json.dumps(
            {"schema": "evalsi.release-report/v1", "decision": decision},
            indent=2,
            ensure_ascii=True,
            allow_nan=False,
        )
        + "\n"
    )
    # Write completely to a private sibling before atomic no-replace publication.
    import tempfile

    destination = Path(path)
    fd, temporary = tempfile.mkstemp(prefix=".evalsi-release-", dir=destination.parent)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as writer:
            writer.write(raw)
            writer.flush()
            os.fsync(writer.fileno())
        os.link(temporary, destination)
    finally:
        Path(temporary).unlink(missing_ok=True)


def _unique_object(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    value: dict[str, Any] = {}
    for key, item in pairs:
        if key in value:
            raise ValueError("duplicate release report field")
        value[key] = item
    return value


def _reject_constant(value: str) -> Any:
    raise ValueError("nonfinite release report number: " + value)


def read_report(path: str | Path, expected: str) -> dict[str, Any]:
    source = Path(path)
    if source.stat().st_size > 4 << 20:
        raise ValueError("release report exceeds 4 MiB")
    raw = json.loads(
        source.read_text(encoding="utf-8"),
        object_pairs_hook=_unique_object,
        parse_constant=_reject_constant,
    )
    if not isinstance(raw, dict) or raw.get("schema") != "evalsi.release-report/v1":
        raise ValueError("unknown release report schema")
    decision = raw.get("decision")
    if not isinstance(decision, dict):
        raise ValueError("release report decision must be an object")
    verify_decision(decision, expected)
    return decision


def gate_candidate(
    client: Client,
    *,
    policy: dict[str, Any],
    policy_sha256: str,
    baseline: dict[str, str],
    spec_file: str | Path,
    candidate_commit: str,
    decision_id: str,
    project: str = "",
) -> dict[str, Any]:
    """One explicit baseline, one candidate execution, one immutable decision."""
    import re

    from evalsi.runspec import load_spec, spec_to_dict

    if policy_hash(policy) != policy_sha256:
        raise ValueError("release policy SHA-256 mismatch")
    if not re.fullmatch(r"[0-9a-f]{7,64}", candidate_commit):
        raise ValueError("candidate commit must be a declared hexadecimal commit")
    run_file = load_spec(spec_file)
    selected_project = project or run_file.project or "default"
    if run_file.spec.dataset.WhichOneof("source") != "catalog":
        raise ValueError("release gates require an approved catalog dataset")
    source = run_file.spec.dataset.catalog
    actual_dataset = {
        "project": source.project or selected_project,
        "name": source.name,
        "version": source.version,
        "sha256": source.sha256,
    }
    parsed = json_format.ParseDict(policy, ReleasePolicy())
    if actual_dataset != json_format.MessageToDict(parsed.dataset):
        raise ValueError("candidate dataset differs from release policy")
    if client.inspect_release_policy(policy) != policy_sha256:
        raise ValueError("server policy digest differs from reviewed policy")
    reviewed = client.release_run_evidence(baseline["runId"])
    if (
        reviewed["reference"] != baseline
        or reviewed["project"] != selected_project
        or reviewed["status"] != "RUN_STATUS_SUCCEEDED"
        or reviewed.get("dataset") != actual_dataset
    ):
        raise ValueError("baseline identity, completion or dataset differs from reviewed evidence")
    request: dict[str, Any] = {
        "spec": spec_to_dict(run_file.spec),
        "labels": {**run_file.labels, "agent.commit": candidate_commit},
        "project": selected_project,
        "name": run_file.name,
    }
    key = hashlib.sha256(
        json.dumps(
            {
                "id": decision_id,
                "policy": policy_sha256,
                "baseline": baseline,
                "candidate": request,
            },
            sort_keys=True,
        ).encode()
    ).hexdigest()
    run = client.create_run(
        request["spec"],
        name=request["name"],
        project=selected_project,
        labels=request["labels"],
        idempotency_key="release-" + key,
    )
    for _ in client.watch_run(run["id"]):
        pass
    candidate = client.release_run_evidence(run["id"])
    decision: dict[str, Any] = client.create_release_decision(
        decision_id,
        project=selected_project,
        policy=policy,
        policy_sha256=policy_sha256,
        baseline=baseline,
        candidate=candidate["reference"],
    )
    verify_decision(decision, decision.get("sha256", ""))
    return decision


class _PolicyLoader(yaml.SafeLoader):
    pass


def _policy_mapping(loader: yaml.SafeLoader, node: yaml.MappingNode) -> dict[str, Any]:
    loader.flatten_mapping(node)
    return _unique_object(loader.construct_pairs(node))


_PolicyLoader.add_constructor(yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG, _policy_mapping)


def load_policy(file: str) -> dict[str, Any]:
    source = Path(file)
    if source.stat().st_size > 2 << 20:
        raise ValueError("release policy exceeds 2 MiB")
    try:
        value = yaml.load(source.read_text(encoding="utf-8"), Loader=_PolicyLoader)
    except yaml.YAMLError as exc:
        raise ValueError("invalid release policy YAML") from exc
    if not isinstance(value, dict):
        raise ValueError("release policy must be an object")
    policy_hash(value)  # Strict protobuf validation before any request.
    return value
