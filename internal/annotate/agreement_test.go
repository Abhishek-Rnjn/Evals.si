package annotate

import (
	"math"
	"math/rand/v2"
	"strconv"
	"testing"
)

// bruteAlpha is Krippendorff's alpha from its definition: the coincidence
// matrix over every ordered pair of values within a unit, weighted 1/(m-1).
func bruteAlpha(units [][]float64, delta func(a, b float64) float64) float64 {
	type pair struct{ a, b float64 }
	o := map[pair]float64{}
	for _, u := range units {
		if len(u) < 2 {
			continue
		}
		for i := range u {
			for j := range u {
				if i != j {
					o[pair{u[i], u[j]}] += 1 / float64(len(u)-1)
				}
			}
		}
	}
	nc := map[float64]float64{}
	n := 0.0
	for p, v := range o {
		nc[p.a] += v
		n += v
	}
	do := 0.0
	for p, v := range o {
		do += v * delta(p.a, p.b)
	}
	de := 0.0
	for c, x := range nc {
		for k, y := range nc {
			if c != k {
				de += x * y * delta(c, k)
			}
		}
	}
	return 1 - (n-1)*do/de
}

func TestKrippendorffAlphaMatchesItsDefinition(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	nominal := func(a, b float64) float64 {
		if a == b {
			return 0
		}
		return 1
	}
	interval := func(a, b float64) float64 { return (a - b) * (a - b) }
	for trial := range 200 {
		var units [][]float64
		for range 2 + r.IntN(15) {
			truth := float64(r.IntN(4))
			var u []float64
			for range 1 + r.IntN(4) {
				v := truth
				if r.Float64() < 0.3 {
					v = float64(r.IntN(4))
				}
				u = append(u, v)
			}
			units = append(units, u)
		}
		var strs [][]string
		for _, u := range units {
			var s []string
			for _, v := range u {
				s = append(s, strconv.FormatFloat(v, 'g', -1, 64))
			}
			strs = append(strs, s)
		}
		if got, ok := alphaNominal(strs); ok {
			if want := bruteAlpha(units, nominal); math.Abs(got-want) > 1e-9 {
				t.Fatalf("trial %d nominal: %v, want %v (%v)", trial, got, want, units)
			}
		}
		if got, ok := alphaInterval(units); ok {
			if want := bruteAlpha(units, interval); math.Abs(got-want) > 1e-9 {
				t.Fatalf("trial %d interval: %v, want %v", trial, got, want)
			}
		}
	}
	if a, ok := alphaNominal([][]string{{"x", "x"}, {"y", "y"}}); !ok || a != 1 {
		t.Errorf("perfect agreement: %v %v", a, ok)
	}
	if _, ok := alphaNominal([][]string{{"x", "x"}, {"x", "x"}}); ok {
		t.Error("alpha without variation should be undefined")
	}
}

func TestAgreementWithAMetric(t *testing.T) {
	// Human pass/fail against a judge: 8 of 10 agree.
	human := []float64{1, 1, 1, 1, 1, 0, 0, 0, 0, 0}
	judge := []float64{1, 1, 1, 1, 0, 0, 0, 0, 0, 1}
	a := agreement(human, judge, true)
	if a.GetN() != 10 || math.Abs(a.GetAccuracy()-0.8) > 1e-12 {
		t.Fatalf("accuracy %v", a)
	}
	// po = 0.8, pe = 0.5*0.5 + 0.5*0.5 = 0.5: kappa = 0.6.
	if math.Abs(a.GetCohenKappa()-0.6) > 1e-12 {
		t.Errorf("kappa %v", a.GetCohenKappa())
	}
	s := agreement([]float64{1, 2, 3, 4}, []float64{2, 4, 6, 8}, false)
	if math.Abs(s.GetPearson()-1) > 1e-12 || math.Abs(s.GetMae()-2.5) > 1e-12 {
		t.Errorf("score agreement %v", s)
	}
}
