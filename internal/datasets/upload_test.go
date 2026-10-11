package datasets

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/parquet-go/parquet-go"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/objstore"
)

func uploadConfig(format string) *evalsiv1alpha1.DatasetUploadConfig {
	return &evalsiv1alpha1.DatasetUploadConfig{Project: "project", Name: "dataset", Format: format}
}

func TestUploadJSONLAndParquet(t *testing.T) {
	for _, format := range []string{"jsonl", "parquet"} {
		t.Run(format, func(t *testing.T) {
			service := &Service{Root: t.TempDir()}
			var input io.Reader = strings.NewReader("{\"id\":\"one\",\"input\":\"question\",\"output\":\"answer\"}\n")
			if format == "parquet" {
				file := filepath.Join(t.TempDir(), "rows.parquet")
				type record struct {
					ID     string `parquet:"id"`
					Input  string `parquet:"input"`
					Output string `parquet:"output"`
				}
				if err := parquet.WriteFile(file, []record{{"one", "question", "answer"}}); err != nil {
					t.Fatal(err)
				}
				reader, err := os.Open(file)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = reader.Close() })
				input = reader
			}
			result, err := service.Put(t.Context(), uploadConfig(format), input)
			if err != nil {
				t.Fatal(err)
			}
			if result.GetRecords() != 1 || len(result.GetSha256()) != 64 || ProjectOf(result.GetPath()) != "project" {
				t.Fatalf("upload result: %+v", result)
			}
			raw, err := os.ReadFile(filepath.Join(service.Root, result.GetPath()))
			if err != nil || !bytes.Contains(raw, []byte(`"input":{"text":"question"}`)) {
				t.Fatalf("canonical record: %s, %v", raw, err)
			}
		})
	}
}

func TestInvalidUploadDoesNotPublish(t *testing.T) {
	for _, input := range []string{"", "[]\n", "{}\n{\"unexpected\":true}\n", "{\"id\":\"duplicate\"}\n{\"id\":\"duplicate\"}\n"} {
		service := &Service{Root: t.TempDir()}
		if _, err := service.Put(t.Context(), uploadConfig("jsonl"), strings.NewReader(input)); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("invalid upload accepted: %v", err)
		}
		entries, err := os.ReadDir(service.Root)
		if err != nil || len(entries) != 0 {
			t.Fatal("invalid upload published files")
		}
	}
	service := &Service{Root: t.TempDir()}
	_, err := service.Put(t.Context(), uploadConfig("jsonl"), strings.NewReader("{}\n{\"unexpected\":true}\n"))
	if err == nil || !strings.Contains(err.Error(), "row 2") || strings.Contains(err.Error(), "unexpected") {
		t.Fatal("validation error omitted row number or echoed raw fields")
	}
	if _, err := service.Put(t.Context(), uploadConfig("parquet"), strings.NewReader("not parquet")); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatal("invalid Parquet upload accepted")
	}
}

type zeroReader struct{}

func (zeroReader) Read(output []byte) (int, error) {
	clear(output)
	return len(output), nil
}

func TestUploadLimitsAndSymlinkIsolation(t *testing.T) {
	service := &Service{Root: t.TempDir()}
	if _, err := service.Put(t.Context(), uploadConfig("jsonl"), io.LimitReader(zeroReader{}, MaxUploadBytes+1)); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatal("oversized upload accepted")
	}
	if _, err := service.Put(t.Context(), uploadConfig("jsonl"), strings.NewReader(strings.Repeat("x", MaxUploadRowBytes+2))); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatal("oversized row accepted")
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(service.Root, "uploaded")); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Put(t.Context(), uploadConfig("jsonl"), strings.NewReader("{}\n")); err == nil {
		t.Fatal("upload escaped datasets_dir through a symlink")
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatal("upload wrote outside datasets_dir")
	}
}

func TestDatasetImportProjectBoundary(t *testing.T) {
	service := &Service{Root: "s3://bucket/root", Objects: &objstore.Client{}}
	for _, uri := range []string{"s3://other/root/incoming/project/data.jsonl", "s3://bucket/root/incoming/other/data.jsonl", "s3://bucket/root/incoming/project/../other/data.jsonl", "https://example.com/data.jsonl"} {
		_, err := service.ImportDataset(t.Context(), connect.NewRequest(&evalsiv1alpha1.ImportDatasetRequest{Config: uploadConfig("jsonl"), Uri: uri}))
		if connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Fatalf("out-of-project import accepted: %v", err)
		}
	}
}
