from __future__ import annotations

import pytest

from evalsi import EvaluatorConfigError, Pack, Record, Registry, Score, evaluator
from evalsi.registry import discover_packs


@evaluator(name="acme/exact-match", version="2.0.0")
def acme_exact(record: Record) -> Score:
    return Score(number=1.0)


def test_builtin_packs_are_discovered() -> None:
    names = {p.name for p in discover_packs()}
    assert {"core", "judge"} <= names


def test_resolution_forms() -> None:
    registry = Registry()
    assert registry.resolve("exact-match").spec.name == "builtin/exact-match"
    assert registry.resolve("builtin/exact-match").spec.pack == "core"
    assert registry.resolve("builtin/exact-match@1.0.0").spec.version == "1.0.0"
    with pytest.raises(EvaluatorConfigError, match="pinned"):
        registry.resolve("builtin/exact-match@2.0.0")
    with pytest.raises(EvaluatorConfigError, match="unknown evaluator"):
        registry.resolve("nothing")


def test_short_names_must_be_unambiguous() -> None:
    registry = Registry(
        [*discover_packs(), Pack(name="acme", description="", evaluators=[acme_exact])]
    )
    with pytest.raises(EvaluatorConfigError, match="ambiguous"):
        registry.resolve("exact-match")
    assert registry.resolve("acme/exact-match").spec.pack == "acme"


def test_duplicate_registration_is_rejected() -> None:
    pack = Pack(name="acme", description="", evaluators=[acme_exact])
    with pytest.raises(EvaluatorConfigError, match="registered twice"):
        Registry([pack, pack])


def test_evaluator_names_must_be_namespaced() -> None:
    with pytest.raises(EvaluatorConfigError, match="namespaced"):
        evaluator(name="plain", version="1.0.0")
