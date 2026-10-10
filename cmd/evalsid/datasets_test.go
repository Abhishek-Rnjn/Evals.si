package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

func TestDatasetsPutLocal(t *testing.T) {
	work := t.TempDir()
	src := filepath.Join(work, "src", "fixtures")
	datasets := filepath.Join(work, "datasets")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(datasets, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(src, "a.jsonl")
	if err := os.WriteFile(file, []byte("{\"id\": 1}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(work, "evalsi.yaml")
	if err := os.WriteFile(cfg, []byte("datasets_dir: "+datasets+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	code := datasetsMain(context.Background(), []string{"put", "--config", cfg, "--dir", filepath.Join(work, "src"), file}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	got, err := os.ReadFile(filepath.Join(datasets, "fixtures", "a.jsonl"))
	if err != nil || string(got) != "{\"id\": 1}\n" {
		t.Fatalf("copied file: %q %v", got, err)
	}
	// A file outside --dir is refused, not copied somewhere surprising.
	other := filepath.Join(work, "outside.jsonl")
	_ = os.WriteFile(other, []byte("x"), 0o644)
	errb.Reset()
	if code := datasetsMain(context.Background(), []string{"put", "--config", cfg, "--dir", filepath.Join(work, "src"), other}, &out, &errb); code == 0 {
		t.Fatal("a file outside --dir was accepted")
	}
	// Relative files with an absolute --dir, as the demo chart's Job runs it:
	// `cd /datasets && evalsid datasets put --dir /datasets fixtures/*.jsonl`.
	t.Chdir(filepath.Join(work, "src"))
	errb.Reset()
	if code := datasetsMain(context.Background(), []string{"put", "--config", cfg, "--dir", filepath.Join(work, "src"), "fixtures/a.jsonl"}, &out, &errb); code != 0 {
		t.Fatalf("relative file, absolute --dir: exit %d: %s", code, errb.String())
	}
	// And a relative --dir.
	t.Chdir(work)
	if code := datasetsMain(context.Background(), []string{"put", "--config", cfg, "--dir", "src", filepath.Join(work, "src", "fixtures", "a.jsonl")}, &out, &errb); code != 0 {
		t.Fatalf("absolute file, relative --dir: exit %d: %s", code, errb.String())
	}
	if code := datasetsMain(context.Background(), []string{"put", "--config", cfg, "--dir", "src", "outside.jsonl"}, &out, &errb); code == 0 {
		t.Fatal("a relative file outside --dir was accepted")
	}
	if code := datasetsMain(context.Background(), []string{"nope"}, &out, &errb); code != 2 {
		t.Fatalf("unknown subcommand exit %d", code)
	}
}

// With S3 (MinIO in CI), the files land under datasets_dir and the server's
// own client reads them back.
func TestDatasetsPutS3(t *testing.T) {
	endpoint := os.Getenv("EVALSI_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("set EVALSI_TEST_S3_ENDPOINT (and _ACCESS_KEY, _SECRET_KEY) to test against S3")
	}
	t.Setenv("TEST_AK", os.Getenv("EVALSI_TEST_S3_ACCESS_KEY"))
	t.Setenv("TEST_SK", os.Getenv("EVALSI_TEST_S3_SECRET_KEY"))
	ctx := context.Background()
	mc, err := minio.New(endpoint, &minio.Options{Creds: credentials.NewStaticV4(os.Getenv("TEST_AK"), os.Getenv("TEST_SK"), ""), Secure: false})
	if err != nil {
		t.Fatal(err)
	}
	bucket := fmt.Sprintf("evalsi-put%d", time.Now().UnixNano())
	if err := mc.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = mc.RemoveObject(ctx, bucket, "datasets/fixtures/a.jsonl", minio.RemoveObjectOptions{})
		_ = mc.RemoveBucket(ctx, bucket)
	})
	work := t.TempDir()
	if err := os.MkdirAll(filepath.Join(work, "fixtures"), 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(work, "fixtures", "a.jsonl")
	if err := os.WriteFile(file, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(work, "evalsi.yaml")
	yaml := fmt.Sprintf("datasets_dir: s3://%s/datasets\nstorage:\n  s3: {endpoint: %s, insecure: true, path_style: true, access_key_env: TEST_AK, secret_key_env: TEST_SK}\n", bucket, endpoint)
	if err := os.WriteFile(cfg, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := datasetsMain(ctx, []string{"put", "--config", cfg, "--dir", work, file}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	obj, err := mc.GetObject(ctx, bucket, "datasets/fixtures/a.jsonl", minio.GetObjectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(obj)
	if err != nil || string(got) != "{}\n" {
		t.Fatalf("object: %q %v", got, err)
	}
}
