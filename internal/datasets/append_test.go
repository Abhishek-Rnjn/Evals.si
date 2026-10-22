package datasets

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// A hand-edited file without its final newline must not get the first
// appended row glued onto its last line.
func TestAppendToFileWithoutTrailingNewline(t *testing.T) {
	root := t.TempDir()
	full := filepath.Join(root, filepath.FromSlash(Path("p", "ds")))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(`{"id":"a"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, n, err := Append(context.Background(), root, nil, "p", "ds", [][]byte{[]byte(`{"id":"b"}`)}); err != nil || n != 1 {
		t.Fatalf("appended %d: %v", n, err)
	}
	got, _ := os.ReadFile(full)
	if want := "{\"id\":\"a\"}\n{\"id\":\"b\"}\n"; string(got) != want {
		t.Fatalf("file is %q, want %q", got, want)
	}
}
