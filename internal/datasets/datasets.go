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
// returns that path relative to datasets_dir. Writers in one process are
// serialized, so lines never interleave. On object storage (datasets_dir
// s3://...), each append is a conditional write retried on conflict, so
// concurrent replicas never lose each other's rows.
func Append(ctx context.Context, root string, objects *objstore.Client, project, name string, rows [][]byte) (string, error) {
	if root == "" {
		return "", fmt.Errorf("promotion needs datasets_dir in the server config")
	}
	if !ValidName(name) {
		return "", fmt.Errorf("dataset name %q must be lowercase letters, digits, '.', '_' or '-'", name)
	}
	if !ValidName(project) {
		return "", fmt.Errorf("project %q cannot name a dataset directory", project)
	}
	rel := Path(project, name)
	if loc, ok := objstore.Parse(root); ok {
		return rel, appendObject(ctx, objects, loc.Join(rel), rows)
	}
	full := filepath.Join(root, filepath.FromSlash(rel))
	mu.Lock()
	defer mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		return "", err
	}
	f, err := os.OpenFile(full, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	defer f.Close()
	for _, row := range rows {
		if _, err := f.Write(append(row, '\n')); err != nil {
			return "", err
		}
	}
	return rel, nil
}

func appendObject(ctx context.Context, objects *objstore.Client, loc objstore.Location, rows [][]byte) error {
	if objects == nil {
		return fmt.Errorf("datasets_dir %s needs storage.s3 in the server config", loc)
	}
	var add []byte
	for _, row := range rows {
		add = append(append(add, row...), '\n')
	}
	for attempt := 0; attempt < 8; attempt++ {
		data, etag, err := objects.Get(ctx, loc)
		create := errors.Is(err, objstore.ErrNotFound)
		if err != nil && !create {
			return err
		}
		_, err = objects.Put(ctx, loc, append(data, add...), etag, create)
		if !errors.Is(err, objstore.ErrConflict) {
			return err
		}
	}
	return fmt.Errorf("appending to %s: too many concurrent writers", loc)
}

// Local reports whether datasets_dir is a local directory (not object storage).
func Local(root string) bool {
	_, remote := objstore.Parse(root)
	return !remote
}
