package webui

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The address someone types first is the server's root: it leads to the UI
// instead of "404 page not found", and only the root does.
func TestRootRedirectsToTheUI(t *testing.T) {
	mux := http.NewServeMux()
	Register(mux)
	for path, want := range map[string]int{"/": http.StatusFound, "/ui": http.StatusMovedPermanently, "/ui/": http.StatusOK, "/nope": http.StatusNotFound} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != want {
			t.Errorf("GET %s = %d, want %d", path, rec.Code, want)
		}
		if want/100 == 3 && rec.Header().Get("Location") != Prefix {
			t.Errorf("GET %s redirects to %q", path, rec.Header().Get("Location"))
		}
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil))
	if rec.Code/100 == 3 {
		t.Errorf("POST / redirected (%d)", rec.Code)
	}
}
