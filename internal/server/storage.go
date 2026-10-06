package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/abhishek-rnjn/evals.si/internal/config"
	"github.com/abhishek-rnjn/evals.si/internal/objstore"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

// openStorage opens the databases the config selects: PostgreSQL or SQLite
// in data_dir, traces in ClickHouse when configured, and the object-storage
// client for an s3:// datasets_dir.
func openStorage(ctx context.Context, cfg config.Config) (*store.Store, *objstore.Client, error) {
	var st *store.Store
	var err error
	if pg := cfg.Storage.Postgres; pg != nil {
		st, err = store.OpenPostgres(ctx, pg.ResolvedDSN())
	} else {
		st, err = store.Open(filepath.Join(cfg.DataDir, "evalsi.db"))
	}
	if err != nil {
		return nil, nil, err
	}
	if ch := cfg.Storage.ClickHouse; ch != nil {
		password := ""
		if ch.PasswordEnv != "" {
			password = os.Getenv(ch.PasswordEnv)
		}
		traces, err := store.OpenClickHouse(ctx, *ch, password)
		if err != nil {
			_ = st.Close()
			return nil, nil, err
		}
		st.UseTraces(traces)
	}
	var objects *objstore.Client
	if s3 := cfg.Storage.S3; s3 != nil {
		if objects, err = objstore.New(*s3); err != nil {
			_ = st.Close()
			return nil, nil, fmt.Errorf("storage.s3: %w", err)
		}
	}
	return st, objects, nil
}
