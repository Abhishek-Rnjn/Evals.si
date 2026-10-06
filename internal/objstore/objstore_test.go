package objstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
)

func TestParse(t *testing.T) {
	l, ok := Parse("s3://bucket/a/b/")
	if !ok || l.Bucket != "bucket" || l.Key != "a/b" || l.Join("c.jsonl").String() != "s3://bucket/a/b/c.jsonl" {
		t.Fatalf("%+v %v", l, ok)
	}
	for _, bad := range []string{"/data", "s3://", "s3:///x", "gs://b/k"} {
		if _, ok := Parse(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
}

// testClient returns a client and a fresh bucket on the S3 server named by
// EVALSI_TEST_S3_ENDPOINT (MinIO in CI), with EVALSI_TEST_S3_ACCESS_KEY and
// EVALSI_TEST_S3_SECRET_KEY.
func testClient(t *testing.T) (*Client, string) {
	t.Helper()
	endpoint := os.Getenv("EVALSI_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("set EVALSI_TEST_S3_ENDPOINT (and _ACCESS_KEY, _SECRET_KEY) to test against S3")
	}
	t.Setenv("TEST_AK", os.Getenv("EVALSI_TEST_S3_ACCESS_KEY"))
	t.Setenv("TEST_SK", os.Getenv("EVALSI_TEST_S3_SECRET_KEY"))
	c, err := New(Config{Endpoint: endpoint, Insecure: true, PathStyle: true, AccessKeyEnv: "TEST_AK", SecretKeyEnv: "TEST_SK"})
	if err != nil {
		t.Fatal(err)
	}
	bucket := fmt.Sprintf("evalsi-t%d", time.Now().UnixNano())
	ctx := context.Background()
	if err := c.mc.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for o := range c.mc.ListObjects(ctx, bucket, minio.ListObjectsOptions{Recursive: true}) {
			_ = c.mc.RemoveObject(ctx, bucket, o.Key, minio.RemoveObjectOptions{})
		}
		_ = c.mc.RemoveBucket(ctx, bucket)
	})
	return c, bucket
}

func TestObjects(t *testing.T) {
	c, bucket := testClient(t)
	ctx := context.Background()
	l := Location{Bucket: bucket, Key: "datasets/qa.jsonl"}
	if _, _, err := c.Get(ctx, l); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing object: %v", err)
	}
	etag, err := c.Put(ctx, l, []byte("a\n"), "", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Put(ctx, l, []byte("x\n"), "", true); !errors.Is(err, ErrConflict) {
		t.Fatalf("create over an existing object: %v", err)
	}
	if _, err := c.Put(ctx, l, []byte("a\nb\n"), etag, false); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Put(ctx, l, []byte("lost\n"), etag, false); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale If-Match: %v", err)
	}
	data, _, err := c.Get(ctx, l)
	if err != nil || string(data) != "a\nb\n" {
		t.Fatalf("Get = %q %v", data, err)
	}

	// Fetch: one object, then a prefix (a task directory).
	cache := t.TempDir()
	local, err := c.Fetch(ctx, l, cache)
	if err != nil || filepath.Base(local) != "qa.jsonl" {
		t.Fatalf("Fetch object = %q %v", local, err)
	}
	if got, _ := os.ReadFile(local); string(got) != "a\nb\n" {
		t.Errorf("fetched %q", got)
	}
	for key, body := range map[string]string{
		"tasks/hello/task.toml": "x", "tasks/hello/tests/test.sh": "y", "tasks/other/task.toml": "z",
	} {
		if _, err := c.Put(ctx, Location{Bucket: bucket, Key: key}, []byte(body), "", false); err != nil {
			t.Fatal(err)
		}
	}
	dir, err := c.Fetch(ctx, Location{Bucket: bucket, Key: "tasks/hello"}, cache)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "tests", "test.sh")); string(got) != "y" {
		t.Errorf("fetched tree: %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "..", "other")); err == nil {
		t.Error("a sibling prefix was fetched")
	}
	again, err := c.Fetch(ctx, Location{Bucket: bucket, Key: "tasks/hello"}, cache)
	if err != nil || again != dir {
		t.Errorf("unchanged prefix refetched: %q %v", again, err)
	}
	// A change gives a new cache entry.
	if _, err := c.Put(ctx, Location{Bucket: bucket, Key: "tasks/hello/task.toml"}, []byte("x2"), "", false); err != nil {
		t.Fatal(err)
	}
	if changed, _ := c.Fetch(ctx, Location{Bucket: bucket, Key: "tasks/hello"}, cache); changed == dir {
		t.Error("stale cache entry reused")
	}
	if _, err := c.Fetch(ctx, Location{Bucket: bucket, Key: "nothing/here"}, cache); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing prefix: %v", err)
	}
}
