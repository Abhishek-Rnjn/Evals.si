"""Confidence intervals for metric aggregates. Standard library only.

- Pass/fail values: Wilson score interval.
- Numbers: Student-t interval.
- Correlated records (``cluster_by``): cluster-robust standard error with a
  t interval on (clusters - 1) degrees of freedom, as recommended for evals
  where many questions share a source.
- ``method="bootstrap"``: percentile bootstrap with a fixed seed.

The Go server (``internal/stats``) implements the same algorithms, including
the splitmix64 generator used by the bootstrap, so the embedded library and
the server report identical intervals. ``testdata/stats_vectors.json`` checks it.
"""

from __future__ import annotations

import math
from collections import defaultdict
from collections.abc import Hashable, Sequence
from dataclasses import dataclass
from statistics import NormalDist, fmean, stdev


class SplitMix64:
    """A tiny, portable PRNG, so bootstrap results match across languages."""

    MASK = (1 << 64) - 1

    def __init__(self, seed: int) -> None:
        self.state = seed & self.MASK

    def next(self) -> int:
        self.state = (self.state + 0x9E3779B97F4A7C15) & self.MASK
        z = self.state
        z = ((z ^ (z >> 30)) * 0xBF58476D1CE4E5B9) & self.MASK
        z = ((z ^ (z >> 27)) * 0x94D049BB133111EB) & self.MASK
        return z ^ (z >> 31)


@dataclass(frozen=True)
class Interval:
    low: float
    high: float
    level: float
    method: str

    def clipped(self, lo: float | None, hi: float | None) -> Interval:
        low = self.low if lo is None else max(lo, self.low)
        high = self.high if hi is None else min(hi, self.high)
        return Interval(low, high, self.level, self.method)


def _beta_continued_fraction(a: float, b: float, x: float) -> float:
    """Lentz's method for the continued fraction of the incomplete beta function."""
    tiny = 1e-300
    c, d = 1.0, 1.0 - (a + b) * x / (a + 1.0)
    d = 1.0 / (d if abs(d) > tiny else tiny)
    h = d
    for m in range(1, 300):
        m2 = 2 * m
        for aa in (
            m * (b - m) * x / ((a - 1.0 + m2) * (a + m2)),
            -(a + m) * (a + b + m) * x / ((a + m2) * (a + 1.0 + m2)),
        ):
            d = 1.0 + aa * d
            d = 1.0 / (d if abs(d) > tiny else tiny)
            c = 1.0 + aa / c
            c = c if abs(c) > tiny else tiny
            h *= d * c
        if abs(d * c - 1.0) < 1e-15:
            break
    return h


def _regularized_beta(a: float, b: float, x: float) -> float:
    if x <= 0.0:
        return 0.0
    if x >= 1.0:
        return 1.0
    log_front = (
        math.lgamma(a + b) - math.lgamma(a) - math.lgamma(b) + a * math.log(x) + b * math.log1p(-x)
    )
    if x < (a + 1.0) / (a + b + 2.0):
        return math.exp(log_front) * _beta_continued_fraction(a, b, x) / a
    return 1.0 - math.exp(log_front) * _beta_continued_fraction(b, a, 1.0 - x) / b


def t_cdf(t: float, df: float) -> float:
    """CDF of Student's t distribution."""
    tail = 0.5 * _regularized_beta(df / 2.0, 0.5, df / (df + t * t))
    return 1.0 - tail if t > 0 else tail


def t_quantile(p: float, df: float) -> float:
    """Quantile of Student's t distribution, by bisection on the exact CDF."""
    if not 0.0 < p < 1.0:
        raise ValueError("p must be in (0, 1)")
    if df <= 0:
        raise ValueError("degrees of freedom must be positive")
    if p < 0.5:
        return -t_quantile(1.0 - p, df)
    low, high = 0.0, 1.0
    while t_cdf(high, df) < p:
        high *= 2.0
    while high - low > 1e-12 * max(1.0, high):
        mid = (low + high) / 2.0
        if t_cdf(mid, df) < p:
            low = mid
        else:
            high = mid
    return (low + high) / 2.0


def wilson(successes: float, n: int, level: float = 0.95) -> Interval:
    z = NormalDist().inv_cdf(0.5 + level / 2)
    p = successes / n
    denom = 1 + z * z / n
    center = (p + z * z / (2 * n)) / denom
    half = z * math.sqrt(p * (1 - p) / n + z * z / (4 * n * n)) / denom
    # At 0 or n successes the bound is exactly 0 or 1; avoid floating-point residue.
    low = 0.0 if successes <= 0 else max(0.0, center - half)
    high = 1.0 if successes >= n else min(1.0, center + half)
    return Interval(low, high, level, "wilson")


def t_interval(values: Sequence[float], level: float = 0.95) -> Interval | None:
    n = len(values)
    if n < 2:
        return None
    mean = fmean(values)
    half = t_quantile(0.5 + level / 2, n - 1) * stdev(values) / math.sqrt(n)
    return Interval(mean - half, mean + half, level, "t")


def clustered_interval(
    values: Sequence[float], clusters: Sequence[Hashable], level: float = 0.95
) -> Interval | None:
    """Cluster-robust interval for the mean.

    SE = sqrt(C / (C - 1) * sum_c (sum_{i in c} (x_i - mean))^2) / n
    """
    n = len(values)
    residual_sums: dict[Hashable, float] = defaultdict(float)
    mean = fmean(values) if n else 0.0
    for value, cluster in zip(values, clusters, strict=True):
        residual_sums[cluster] += value - mean
    c = len(residual_sums)
    if c < 2:
        return None
    se = math.sqrt(c / (c - 1) * sum(s * s for s in residual_sums.values())) / n
    half = t_quantile(0.5 + level / 2, c - 1) * se
    return Interval(mean - half, mean + half, level, "clustered-t")


def bootstrap_interval(
    values: Sequence[float],
    level: float = 0.95,
    *,
    clusters: Sequence[Hashable] | None = None,
    resamples: int = 2000,
    seed: int = 0,
) -> Interval | None:
    """Percentile bootstrap of the mean; resamples whole clusters when given."""
    if len(values) < 2:
        return None
    rng = SplitMix64(seed)
    groups: list[list[float]]
    if clusters is None:
        groups = [[v] for v in values]
    else:
        by_cluster: dict[Hashable, list[float]] = defaultdict(list)
        for value, cluster in zip(values, clusters, strict=True):
            by_cluster[cluster].append(value)
        groups = list(by_cluster.values())
        if len(groups) < 2:
            return None
    means = []
    for _ in range(resamples):
        sample = [groups[rng.next() % len(groups)] for _ in groups]
        total = sum(sum(g) for g in sample)
        count = sum(len(g) for g in sample)
        means.append(total / count)
    means.sort()
    alpha = (1 - level) / 2
    low = means[math.floor(alpha * (resamples - 1))]
    high = means[math.ceil((1 - alpha) * (resamples - 1))]
    return Interval(low, high, level, "bootstrap")


def interval(
    values: Sequence[float],
    *,
    proportion: bool,
    level: float = 0.95,
    clusters: Sequence[Hashable] | None = None,
    method: str = "auto",
) -> Interval | None:
    """Pick the right interval for a metric's values."""
    if not values:
        return None
    if method == "bootstrap":
        return bootstrap_interval(values, level, clusters=clusters)
    if method != "auto":
        raise ValueError(f"unknown CI method {method!r}; use 'auto' or 'bootstrap'")
    if clusters is not None:
        return clustered_interval(values, clusters, level)
    if proportion:
        return wilson(sum(values), len(values), level)
    return t_interval(values, level)
