package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/abhishek-rnjn/evals.si/internal/config"
	"github.com/abhishek-rnjn/evals.si/internal/objstore"
)

const datasetsUsage = `usage: evalsid datasets put [--config evalsi.yaml] [--dir DIR] FILE...

Copies local files into the server's datasets_dir, keeping each file's path
relative to --dir (default: the current directory), so a run's
"path: fixtures/a.jsonl" finds it. On Kubernetes datasets_dir is S3, which the
server's pod cannot read local files into; this is how a dataset gets there
(run it with the server's config and S3 credentials).
`

// datasetsMain is `evalsid datasets`.
func datasetsMain(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "put" {
		fmt.Fprint(stderr, datasetsUsage)
		return 2
	}
	fs := flag.NewFlagSet("datasets put", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "the server's evalsi.yaml (default: evalsi.yaml if it exists)")
	dir := fs.String("dir", ".", "paths are kept relative to this directory")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() == 0 {
		fmt.Fprint(stderr, datasetsUsage)
		return 2
	}
	if *cfgPath == "" {
		if _, err := os.Stat("evalsi.yaml"); err == nil {
			*cfgPath = "evalsi.yaml"
		}
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "evalsid: %v\n", err)
		return 1
	}
	if cfg.DatasetsDir == "" {
		fmt.Fprintln(stderr, "evalsid: the config has no datasets_dir")
		return 1
	}
	put, err := datasetWriter(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "evalsid: %v\n", err)
		return 1
	}
	// Relative paths, of the files or of --dir, are relative to the working
	// directory: `cd /datasets && evalsid datasets put --dir /datasets
	// fixtures/a.jsonl` names /datasets/fixtures/a.jsonl.
	base, err := filepath.Abs(*dir)
	if err != nil {
		fmt.Fprintf(stderr, "evalsid: %v\n", err)
		return 1
	}
	for _, file := range fs.Args() {
		abs, err := filepath.Abs(file)
		if err != nil {
			fmt.Fprintf(stderr, "evalsid: %v\n", err)
			return 1
		}
		rel, err := filepath.Rel(base, abs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			fmt.Fprintf(stderr, "evalsid: %s is not under --dir %s\n", file, *dir)
			return 1
		}
		data, err := os.ReadFile(file)
		if err != nil {
			fmt.Fprintf(stderr, "evalsid: %v\n", err)
			return 1
		}
		where, err := put(ctx, filepath.ToSlash(rel), data)
		if err != nil {
			fmt.Fprintf(stderr, "evalsid: %s: %v\n", file, err)
			return 1
		}
		fmt.Fprintf(stdout, "%s -> %s\n", file, where)
	}
	return 0
}

// datasetWriter writes a file under datasets_dir: an S3 object or a local file.
func datasetWriter(cfg config.Config) (func(ctx context.Context, rel string, data []byte) (string, error), error) {
	if loc, ok := objstore.Parse(cfg.DatasetsDir); ok {
		if cfg.Storage.S3 == nil {
			return nil, fmt.Errorf("datasets_dir %q needs storage.s3", cfg.DatasetsDir)
		}
		client, err := objstore.New(*cfg.Storage.S3)
		if err != nil {
			return nil, err
		}
		return func(ctx context.Context, rel string, data []byte) (string, error) {
			dst := loc.Join(path.Clean(rel))
			if _, err := client.Put(ctx, dst, data, "", false); err != nil {
				return "", err
			}
			return dst.String(), nil
		}, nil
	}
	return func(_ context.Context, rel string, data []byte) (string, error) {
		dst := filepath.Join(cfg.DatasetsDir, filepath.FromSlash(path.Clean(rel)))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return "", err
		}
		return dst, os.WriteFile(dst, data, 0o644)
	}, nil
}
