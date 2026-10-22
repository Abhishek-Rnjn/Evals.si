package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/abhishek-rnjn/evals.si/internal/quality"
)

func TestQualityEndpointAuthAndValidation(t *testing.T) {
	s := startAuthServer(t, nil)
	get := func(who, query string) (int, string) {
		code, body, _ := s.rest(t, s.keys[who], http.MethodGet, "/v1alpha1/quality"+query, "")
		return code, string(body)
	}
	if code, body := get("owner", "?policy=prod&group_by=label:route&threshold=0.8&metric=judge"); code != http.StatusOK {
		t.Fatalf("owner: %d %s", code, body)
	} else {
		var r quality.Report
		if err := json.Unmarshal([]byte(body), &r); err != nil || r.Policy != "prod" || r.GroupBy != "label:route" || len(r.Groups) != 0 || r.WindowSeconds != 3600 {
			t.Fatalf("report: %+v %v (%s)", r, err, body)
		}
	}
	for name, tc := range map[string]struct {
		query string
		code  int
	}{
		"no policy":       {"", http.StatusBadRequest},
		"unknown param":   {"?policy=p&bogus=1", http.StatusBadRequest},
		"bad group_by":    {"?policy=p&group_by=route", http.StatusBadRequest},
		"bad window":      {"?policy=p&window=soon", http.StatusBadRequest},
		"threshold alone": {"?policy=p&threshold=0.5", http.StatusBadRequest},
		"bad threshold":   {"?policy=p&metric=m&threshold=high", http.StatusBadRequest},
	} {
		if code, body := get("owner", tc.query); code != tc.code {
			t.Errorf("%s: %d %s, want %d", name, code, body, tc.code)
		}
	}
	if code, _ := get("nobody", "?policy=p"); code != http.StatusUnauthorized {
		t.Errorf("unknown key: %d", code)
	}
}
