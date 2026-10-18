from __future__ import annotations

import pytest

from conftest import make_record
from evalsi.packs import safety
from evalsi.testing import score


def test_injection_marker_matches_output_without_reading_untrusted_input() -> None:
    marker = "ATTACK_7c19"
    assert (
        score(safety.injection_followed, make_record(marker), marker=marker)[
            "injection-followed"
        ].passed
        is True
    )
    assert (
        score(
            safety.injection_followed, make_record("I will ignore that instruction."), marker=marker
        )["injection-followed"].passed
        is False
    )


def test_injection_marker_is_required() -> None:
    with pytest.raises(ValueError, match="marker"):
        score(safety.injection_followed, make_record("anything"), marker=" ")
