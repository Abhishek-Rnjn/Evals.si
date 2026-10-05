from __future__ import annotations

import math

import pytest

from evalsi import stats

# Reference values from standard t tables.
T_TABLE = {
    (0.975, 1): 12.7062,
    (0.975, 2): 4.3027,
    (0.975, 3): 3.1824,
    (0.975, 5): 2.5706,
    (0.975, 10): 2.2281,
    (0.975, 30): 2.0423,
    (0.995, 3): 5.8409,
    (0.95, 20): 1.7247,
}


@pytest.mark.parametrize(("key", "expected"), T_TABLE.items())
def test_t_quantile_matches_tables(key: tuple[float, int], expected: float) -> None:
    p, df = key
    assert stats.t_quantile(p, df) == pytest.approx(expected, abs=1e-4)


def test_t_quantile_is_symmetric_and_converges_to_normal() -> None:
    assert stats.t_quantile(0.025, 7) == pytest.approx(-stats.t_quantile(0.975, 7))
    assert stats.t_quantile(0.975, 1e6) == pytest.approx(1.959964, abs=1e-4)


def test_wilson_interval() -> None:
    ci = stats.wilson(2, 3)
    assert (ci.low, ci.high) == pytest.approx((0.2077, 0.9385), abs=1e-4)
    assert stats.wilson(0, 10).low == 0.0
    assert stats.wilson(10, 10).high == 1.0


def test_t_interval() -> None:
    ci = stats.t_interval([1.0, 2.0, 3.0, 4.0])
    assert ci is not None
    half = 3.1824 * math.sqrt(5 / 3) / 2
    assert (ci.low, ci.high) == pytest.approx((2.5 - half, 2.5 + half), abs=1e-3)
    assert stats.t_interval([1.0]) is None


def test_clustered_interval_is_wider_when_clusters_are_correlated() -> None:
    values = [1.0, 1.0, 1.0, 0.0, 0.0, 0.0, 1.0, 1.0, 0.0, 0.0]
    clusters = ["a", "a", "a", "b", "b", "b", "c", "c", "d", "d"]
    plain = stats.t_interval(values)
    clustered = stats.clustered_interval(values, clusters)
    assert plain is not None
    assert clustered is not None
    assert clustered.method == "clustered-t"
    assert (clustered.high - clustered.low) > (plain.high - plain.low)
    assert stats.clustered_interval(values, ["a"] * len(values)) is None


def test_bootstrap_is_deterministic() -> None:
    values = [0.1, 0.4, 0.5, 0.9, 0.3, 0.7]
    first = stats.bootstrap_interval(values)
    assert first == stats.bootstrap_interval(values)
    assert first is not None
    assert first.low < sum(values) / len(values) < first.high


def test_interval_dispatch() -> None:
    assert stats.interval([], proportion=True) is None
    assert stats.interval([1.0, 0.0], proportion=True).method == "wilson"  # type: ignore[union-attr]
    assert stats.interval([1.0, 2.0], proportion=False).method == "t"  # type: ignore[union-attr]
    with pytest.raises(ValueError, match="unknown CI method"):
        stats.interval([1.0], proportion=False, method="magic")
