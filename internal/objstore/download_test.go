package objstore

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// S3 consoles create zero-byte "folder/" keys. They are not files: writing
// one would make its name a file and break every key nested under it.
func TestDownloadSkipsFolderMarkers(t *testing.T) {
	bodies := map[string]string{"/bkt/ds/sub/": "", "/bkt/ds/sub/a.jsonl": "{}\n"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := bodies[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Header().Set("ETag", `"e"`)
		w.Header().Set("Last-Modified", "Mon, 02 Jan 2006 15:04:05 GMT")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(body))
		}
	}))
	defer srv.Close()
	t.Setenv("TEST_AK", "ak")
	t.Setenv("TEST_SK", "sk")
	c, err := New(Config{Endpoint: strings.TrimPrefix(srv.URL, "http://"), Insecure: true, PathStyle: true, Region: "us-east-1", AccessKeyEnv: "TEST_AK", SecretKeyEnv: "TEST_SK"})
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "out")
	objects := []Object{{Key: "ds/sub/", Size: 0}, {Key: "ds/sub/a.jsonl", Size: 3}}
	if err := c.download(context.Background(), "bkt", objects, "ds", dest); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(dest, "sub", "a.jsonl")); err != nil || string(got) != "{}\n" {
		t.Fatalf("nested object: %q %v", got, err)
	}
}
