// Package stats computes confidence intervals for metric aggregates.
//
// It mirrors evalsi/stats.py in the Python SDK, including the splitmix64
// generator used by the bootstrap, so the server and the embedded library
// report identical intervals. testdata/stats_vectors.json holds the shared
// golden values both test suites check.
package stats

import (
	"errors"
	"fmt"
	"math"
	"sort"
)

// Interval is a confidence interval for a mean.
type Interval struct {
	Low    float64
	High   float64
	Level  float64
	Method string
}

// Clipped bounds the interval to [lo, hi]; nil leaves that side open.
func (iv Interval) Clipped(lo, hi *float64) Interval {
	if lo != nil {
		iv.Low = math.Max(*lo, iv.Low)
	}
	if hi != nil {
		iv.High = math.Min(*hi, iv.High)
	}
	return iv
}

func betaContinuedFraction(a, b, x float64) float64 {
	const tiny = 1e-300
	clamp := func(v float64) float64 {
		if math.Abs(v) > tiny {
			return v
		}
		return tiny
	}
	c := 1.0
	d := 1.0 / clamp(1.0-(a+b)*x/(a+1.0))
	h := d
	for m := 1; m < 300; m++ {
		fm := float64(m)
		m2 := 2 * fm
		for _, aa := range [2]float64{
			fm * (b - fm) * x / ((a - 1.0 + m2) * (a + m2)),
			-(a + fm) * (a + b + fm) * x / ((a + m2) * (a + 1.0 + m2)),
		} {
			d = 1.0 / clamp(1.0+aa*d)
			c = clamp(1.0 + aa/c)
			h *= d * c
		}
		if math.Abs(d*c-1.0) < 1e-15 {
			break
		}
	}
	return h
}

func regularizedBeta(a, b, x float64) float64 {
	if x <= 0 {
		return 0
	}
	if x >= 1 {
		return 1
	}
	lgab, _ := math.Lgamma(a + b)
	lga, _ := math.Lgamma(a)
	lgb, _ := math.Lgamma(b)
	logFront := lgab - lga - lgb + a*math.Log(x) + b*math.Log1p(-x)
	if x < (a+1.0)/(a+b+2.0) {
		return math.Exp(logFront) * betaContinuedFraction(a, b, x) / a
	}
	return 1.0 - math.Exp(logFront)*betaContinuedFraction(b, a, 1.0-x)/b
}

// TCDF is the CDF of Student's t distribution with df degrees of freedom.
func TCDF(t, df float64) float64 {
	tail := 0.5 * regularizedBeta(df/2.0, 0.5, df/(df+t*t))
	if t > 0 {
		return 1.0 - tail
	}
	return tail
}

// TQuantile is the p-quantile of Student's t distribution, by bisection on the exact CDF.
func TQuantile(p, df float64) float64 {
	if !(p > 0 && p < 1) || !(df > 0) {
		panic(fmt.Sprintf("stats: TQuantile(%v, %v) out of domain", p, df))
	}
	if p < 0.5 {
		return -TQuantile(1.0-p, df)
	}
	low, high := 0.0, 1.0
	for TCDF(high, df) < p {
		high *= 2.0
	}
	for high-low > 1e-12*math.Max(1.0, high) {
		mid := (low + high) / 2.0
		if TCDF(mid, df) < p {
			low = mid
		} else {
			high = mid
		}
	}
	return (low + high) / 2.0
}

func normalQuantile(p float64) float64 {
	return math.Sqrt2 * math.Erfinv(2*p-1)
}

// Wilson is the Wilson score interval for a proportion.
func Wilson(successes float64, n int, level float64) Interval {
	z := normalQuantile(0.5 + level/2)
	fn := float64(n)
	p := successes / fn
	denom := 1 + z*z/fn
	center := (p + z*z/(2*fn)) / denom
	half := z * math.Sqrt(p*(1-p)/fn+z*z/(4*fn*fn)) / denom
	low, high := math.Max(0, center-half), math.Min(1, center+half)
	if successes <= 0 {
		low = 0
	}
	if successes >= fn {
		high = 1
	}
	return Interval{Low: low, High: high, Level: level, Method: "wilson"}
}

func mean(values []float64) float64 {
	sum := 0.0
	for _, v := range values {
		sum += v
	}
	return sum / float64(len(values))
}

// StdDev is the sample standard deviation.
func StdDev(values []float64) float64 {
	m := mean(values)
	ss := 0.0
	for _, v := range values {
		ss += (v - m) * (v - m)
	}
	return math.Sqrt(ss / float64(len(values)-1))
}

// Mean is the arithmetic mean.
func Mean(values []float64) float64 { return mean(values) }

// TInterval is the Student-t interval for the mean; ok is false below two values.
func TInterval(values []float64, level float64) (Interval, bool) {
	n := len(values)
	if n < 2 {
		return Interval{}, false
	}
	m := mean(values)
	half := TQuantile(0.5+level/2, float64(n-1)) * StdDev(values) / math.Sqrt(float64(n))
	return Interval{Low: m - half, High: m + half, Level: level, Method: "t"}, true
}

// ClusteredInterval is the cluster-robust interval for the mean, with a t
// quantile on (clusters - 1) degrees of freedom. ok is false below two clusters.
func ClusteredInterval(values []float64, clusters []string, level float64) (Interval, bool) {
	n := len(values)
	if n == 0 || len(clusters) != n {
		return Interval{}, false
	}
	m := mean(values)
	order := []string{}
	sums := map[string]float64{}
	for i, v := range values {
		if _, seen := sums[clusters[i]]; !seen {
			order = append(order, clusters[i])
		}
		sums[clusters[i]] += v - m
	}
	c := len(order)
	if c < 2 {
		return Interval{}, false
	}
	ss := 0.0
	for _, k := range order {
		ss += sums[k] * sums[k]
	}
	se := math.Sqrt(float64(c)/float64(c-1)*ss) / float64(n)
	half := TQuantile(0.5+level/2, float64(c-1)) * se
	return Interval{Low: m - half, High: m + half, Level: level, Method: "clustered-t"}, true
}

// SplitMix64 is a tiny, portable PRNG; see the package comment.
type SplitMix64 struct{ state uint64 }

// Next returns the next 64-bit value.
func (s *SplitMix64) Next() uint64 {
	s.state += 0x9E3779B97F4A7C15
	z := s.state
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}

// BootstrapInterval is a percentile bootstrap of the mean, resampling whole
// clusters when clusters is non-nil.
func BootstrapInterval(values []float64, level float64, clusters []string, resamples int, seed uint64) (Interval, bool) {
	if len(values) < 2 {
		return Interval{}, false
	}
	var groups [][]float64
	if clusters == nil {
		for _, v := range values {
			groups = append(groups, []float64{v})
		}
	} else {
		index := map[string]int{}
		for i, v := range values {
			k, ok := index[clusters[i]]
			if !ok {
				k = len(groups)
				index[clusters[i]] = k
				groups = append(groups, nil)
			}
			groups[k] = append(groups[k], v)
		}
		if len(groups) < 2 {
			return Interval{}, false
		}
	}
	rng := SplitMix64{state: seed}
	means := make([]float64, resamples)
	g := uint64(len(groups))
	for r := range means {
		total, count := 0.0, 0
		// Mirror Python: draw the whole sample first, then sum group by group.
		sample := make([][]float64, len(groups))
		for i := range sample {
			sample[i] = groups[rng.Next()%g]
		}
		for _, grp := range sample {
			s := 0.0
			for _, v := range grp {
				s += v
			}
			total += s
			count += len(grp)
		}
		means[r] = total / float64(count)
	}
	sort.Float64s(means)
	alpha := (1 - level) / 2
	low := means[int(math.Floor(alpha*float64(resamples-1)))]
	high := means[int(math.Ceil((1-alpha)*float64(resamples-1)))]
	return Interval{Low: low, High: high, Level: level, Method: "bootstrap"}, true
}

// ErrUnknownMethod is returned for a CI method other than "auto" or "bootstrap".
var ErrUnknownMethod = errors.New("stats: unknown CI method; use auto or bootstrap")

// ForMetric picks the right interval for a metric's values, like stats.interval in Python.
func ForMetric(values []float64, proportion bool, level float64, clusters []string, method string) (Interval, bool, error) {
	if len(values) == 0 {
		return Interval{}, false, nil
	}
	switch method {
	case "bootstrap":
		iv, ok := BootstrapInterval(values, level, clusters, 2000, 0)
		return iv, ok, nil
	case "auto", "":
	default:
		return Interval{}, false, ErrUnknownMethod
	}
	if clusters != nil {
		iv, ok := ClusteredInterval(values, clusters, level)
		return iv, ok, nil
	}
	if proportion {
		sum := 0.0
		for _, v := range values {
			sum += v
		}
		return Wilson(sum, len(values), level), true, nil
	}
	iv, ok := TInterval(values, level)
	return iv, ok, nil
}
