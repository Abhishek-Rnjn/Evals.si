// Package datasets writes promoted datasets: JSONL files of records under
// datasets_dir that run specs and `evalsi eval` load like any dataset.
//
// Promotions are scoped by project, promoted/<project>/<name>.jsonl, so a
// project's production traces and run results never land in another
// project's datasets. Reading them in a run is authorized like reading the
// project's runs (see ProjectOf).
package datasets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"google.golang.org/protobuf/encoding/protojson"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/objstore"
)

// PromotedDir is the directory, relative to datasets_dir, that holds promotions.
const PromotedDir = "promoted"

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

// ValidName reports whether a dataset or project name is safe as a file name.
func ValidName(name string) bool { return nameRE.MatchString(name) && !strings.Contains(name, "..") }

// Path is a promoted dataset's path relative to datasets_dir.
func Path(project, name string) string {
	return path.Join(PromotedDir, project, name+".jsonl")
}

// ProjectOf returns the project a dataset path (relative to datasets_dir)
// was promoted from, or "" when it is not a promotion.
func ProjectOf(rel string) string {
	parts := strings.Split(path.Clean(filepath.ToSlash(rel)), "/")
	if len(parts) >= 3 && parts[0] == PromotedDir {
		return parts[1]
	}
	return ""
}

// Row is one record in its JSON form, plus extra metadata.
func Row(rec *evalsiv1alpha1.Record, extra map[string]any) ([]byte, error) {
	raw, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(rec)
	if err != nil {
		return nil, err
	}
	var row map[string]any
	if err := json.Unmarshal(raw, &row); err != nil {
		return nil, err
	}
	if len(extra) > 0 {
		meta, _ := row["metadata"].(map[string]any)
		if meta == nil {
			meta = map[string]any{}
		}
		for k, v := range extra {
			meta[k] = v
		}
		row["metadata"] = meta
	}
	return json.Marshal(row)
}

var mu sync.Mutex

// Append adds rows to datasets_dir/promoted/<project>/<name>.jsonl and
// returns that path relative to datasets_dir and how many rows it added. A
// row whose record ID the dataset already has is skipped, so the dataset
// stays loadable (IDs are unique) and repeating a promotion adds nothing.
// Writers in one process are serialized, so lines never interleave. On
// object storage (datasets_dir s3://...), each append is a conditional write
// retried on conflict, so concurrent replicas never lose each other's rows.
func Append(ctx context.Context, root string, objects *objstore.Client, project, name string, rows [][]byte) (string, int, error) {
	if root == "" {
		return "", 0, fmt.Errorf("promotion needs datasets_dir in the server config")
	}
	if !ValidName(name) {
		return "", 0, fmt.Errorf("dataset name %q must be lowercase letters, digits, '.', '_' or '-'", name)
	}
	if !ValidName(project) {
		return "", 0, fmt.Errorf("project %q cannot name a dataset directory", project)
	}
	rel := Path(project, name)
	if loc, ok := objstore.Parse(root); ok {
		n, err := appendObject(ctx, objects, loc.Join(rel), rows)
		return rel, n, err
	}
	full := filepath.Join(root, filepath.FromSlash(rel))
	mu.Lock()
	defer mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		return "", 0, err
	}
	existing, err := os.ReadFile(full)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", 0, err
	}
	add := newRows(existing, rows)
	if len(add) == 0 {
		return rel, 0, nil
	}
	f, err := os.OpenFile(full, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	for _, row := range add {
		if _, err := f.Write(append(row, '\n')); err != nil {
			return "", 0, err
		}
	}
	return rel, len(add), nil
}

// newRows are the rows whose record IDs neither the existing JSONL nor an
// earlier row has. Rows without an ID are always new.
func newRows(existing []byte, rows [][]byte) [][]byte {
	seen := map[string]bool{}
	for _, line := range strings.Split(string(existing), "\n") {
		if id := rowID([]byte(line)); id != "" {
			seen[id] = true
		}
	}
	var out [][]byte
	for _, row := range rows {
		id := rowID(row)
		if id != "" && seen[id] {
			continue
		}
		if id != "" {
			seen[id] = true
		}
		out = append(out, row)
	}
	return out
}

func rowID(line []byte) string {
	var r struct {
		ID string `json:"id"`
	}
	if len(strings.TrimSpace(string(line))) == 0 || json.Unmarshal(line, &r) != nil {
		return ""
	}
	return r.ID
}

func appendObject(ctx context.Context, objects *objstore.Client, loc objstore.Location, rows [][]byte) (int, error) {
	if objects == nil {
		return 0, fmt.Errorf("datasets_dir %s needs storage.s3 in the server config", loc)
	}
	for attempt := 0; attempt < 8; attempt++ {
		data, etag, err := objects.Get(ctx, loc)
		create := errors.Is(err, objstore.ErrNotFound)
		if err != nil && !create {
			return 0, err
		}
		rows := newRows(data, rows)
		if len(rows) == 0 {
			return 0, nil
		}
		var add []byte
		for _, row := range rows {
			add = append(append(add, row...), '\n')
		}
		_, err = objects.Put(ctx, loc, append(data, add...), etag, create)
		if !errors.Is(err, objstore.ErrConflict) {
			return len(rows), err
		}
	}
	return 0, fmt.Errorf("appending to %s: too many concurrent writers", loc)
}

// Local reports whether datasets_dir is a local directory (not object storage).
func Local(root string) bool {
	_, remote := objstore.Parse(root)
	return !remote
}
