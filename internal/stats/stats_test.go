package stats

import (
	"encoding/json"
	"math"
	"os"
	"testing"
)

type vectors struct {
	TQuantile []struct {
		P, DF, Want float64
	} `json:"t_quantile"`
	Intervals []struct {
		Name       string    `json:"name"`
		Values     []float64 `json:"values"`
		Proportion bool      `json:"proportion"`
		Level      float64   `json:"level"`
		Clusters   []string  `json:"clusters"`
		Method     string    `json:"method"`
		Want       *struct {
			Low, High, Level float64
			Method           string
		} `json:"want"`
	} `json:"intervals"`
}

// TestSharedVectors checks the Go implementation against values produced by
// the Python SDK (scripts/gen-stats-vectors.py).
func TestSharedVectors(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/stats_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	for _, c := range v.TQuantile {
		if got := TQuantile(c.P, c.DF); math.Abs(got-c.Want) > 1e-9 {
			t.Errorf("TQuantile(%v, %v) = %v, want %v", c.P, c.DF, got, c.Want)
		}
	}
	for _, c := range v.Intervals {
		t.Run(c.Name, func(t *testing.T) {
			got, ok, err := ForMetric(c.Values, c.Proportion, c.Level, c.Clusters, c.Method)
			if err != nil {
				t.Fatal(err)
			}
			if c.Want == nil {
				if ok {
					t.Fatalf("got %+v, want no interval", got)
				}
				return
			}
			if !ok {
				t.Fatalf("got no interval, want %+v", *c.Want)
			}
			if math.Abs(got.Low-c.Want.Low) > 1e-9 || math.Abs(got.High-c.Want.High) > 1e-9 || got.Method != c.Want.Method {
				t.Errorf("got %+v, want %+v", got, *c.Want)
			}
		})
	}
}

func TestEdgeCases(t *testing.T) {
	if w := Wilson(0, 10, 0.95); w.Low != 0 {
		t.Errorf("Wilson(0, 10).Low = %v, want 0", w.Low)
	}
	if _, ok, _ := ForMetric(nil, true, 0.95, nil, "auto"); ok {
		t.Error("expected no interval for no values")
	}
	if _, _, err := ForMetric([]float64{1}, false, 0.95, nil, "magic"); err == nil {
		t.Error("expected an error for an unknown method")
	}
	lo, hi := 0.0, 1.0
	if c := (Interval{Low: -0.2, High: 1.3}).Clipped(&lo, &hi); c.Low != 0 || c.High != 1 {
		t.Errorf("Clipped = %+v", c)
	}
}
