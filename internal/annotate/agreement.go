package annotate

import (
	"math"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

// alphaNominal is Krippendorff's alpha for nominal values. Units are the
// values given for one item (only units with two or more count).
func alphaNominal(units [][]string) (float64, bool) {
	total := map[string]float64{}
	n, disagree := 0.0, 0.0
	for _, u := range units {
		if len(u) < 2 {
			continue
		}
		counts := map[string]float64{}
		for _, v := range u {
			counts[v]++
			total[v]++
		}
		m := float64(len(u))
		pairs := m * m
		for _, c := range counts {
			pairs -= c * c
		}
		// Observed disagreeing ordered pairs, weighted by 1/(m-1).
		disagree += pairs / (m - 1)
		n += m
	}
	if n < 2 {
		return 0, false
	}
	expected := n * n
	for _, c := range total {
		expected -= c * c
	}
	if expected == 0 {
		return 0, false // no variation at all: alpha is undefined
	}
	do := disagree / n
	de := expected / (n * (n - 1))
	return 1 - do/de, true
}

// alphaInterval is Krippendorff's alpha for interval values (squared differences).
func alphaInterval(units [][]float64) (float64, bool) {
	// Sum over ordered pairs i != j of (x_i - x_j)^2 = 2m*Σx² - 2(Σx)².
	pairSq := func(xs []float64) float64 {
		var s, s2 float64
		for _, x := range xs {
			s += x
			s2 += x * x
		}
		return 2*float64(len(xs))*s2 - 2*s*s
	}
	var all []float64
	disagree := 0.0
	for _, u := range units {
		if len(u) < 2 {
			continue
		}
		disagree += pairSq(u) / float64(len(u)-1)
		all = append(all, u...)
	}
	n := float64(len(all))
	if n < 2 {
		return 0, false
	}
	expected := pairSq(all) / (n * (n - 1))
	if expected == 0 {
		return 0, false
	}
	return 1 - (disagree/n)/expected, true
}

// agreement compares human answers (per item) with a run metric.
func agreement(human, machine []float64, binary bool) *evalsiv1alpha1.HumanAgreement {
	out := &evalsiv1alpha1.HumanAgreement{N: int64(len(human))}
	n := float64(len(human))
	if binary {
		var agree, h1, m1 float64
		for i := range human {
			h, m := human[i] >= 0.5, machine[i] >= 0.5
			if h == m {
				agree++
			}
			if h {
				h1++
			}
			if m {
				m1++
			}
		}
		acc := agree / n
		out.Accuracy = &acc
		pe := (h1/n)*(m1/n) + (1-h1/n)*(1-m1/n)
		if pe < 1 {
			k := (acc - pe) / (1 - pe)
			out.CohenKappa = &k
		}
		return out
	}
	var mae float64
	for i := range human {
		mae += math.Abs(human[i] - machine[i])
	}
	mae /= n
	out.Mae = &mae
	if r, ok := pearson(human, machine); ok {
		out.Pearson = &r
	}
	return out
}

func pearson(x, y []float64) (float64, bool) {
	if len(x) < 2 {
		return 0, false
	}
	var mx, my float64
	for i := range x {
		mx += x[i]
		my += y[i]
	}
	mx /= float64(len(x))
	my /= float64(len(y))
	var sxy, sxx, syy float64
	for i := range x {
		dx, dy := x[i]-mx, y[i]-my
		sxy += dx * dy
		sxx += dx * dx
		syy += dy * dy
	}
	if sxx == 0 || syy == 0 {
		return 0, false
	}
	return sxy / math.Sqrt(sxx*syy), true
}
