package store

import (
	"context"
	"fmt"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

func archiveMarker(run *evalsiv1alpha1.Run) string {
	if run.GetArchivedAt() != nil {
		return "1"
	}
	return ""
}

// migrateArchivedRuns adds an indexed quota marker and backfills protobuf
// summaries written before the marker existed. Call inside the schema lock
// on PostgreSQL; SQLite's startup uses a single connection.
func migrateArchivedRuns(ctx context.Context, q querier, d dialect) error {
	var exists int
	query := `SELECT COUNT(*) FROM pragma_table_info('runs') WHERE name = 'archived'`
	if d == postgresDialect {
		query = `SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = 'runs' AND column_name = 'archived'`
	}
	if err := q.QueryRowContext(ctx, query).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		if err := addColumn(ctx, q, d, "runs", "archived"); err != nil {
			return err
		}
		rows, err := q.QueryContext(ctx, `SELECT id, run FROM runs`)
		if err != nil {
			return err
		}
		var archived []string
		for rows.Next() {
			var id string
			var blob []byte
			if err := rows.Scan(&id, &blob); err != nil {
				_ = rows.Close()
				return err
			}
			run, err := unmarshalRun(blob)
			if err != nil {
				_ = rows.Close()
				return fmt.Errorf("backfilling archived run %s: %w", id, err)
			}
			if run.GetArchivedAt() != nil {
				archived = append(archived, id)
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, id := range archived {
			if _, err := q.ExecContext(ctx, d.rebind(`UPDATE runs SET archived = '1' WHERE id = ?`), id); err != nil {
				return err
			}
		}
	}
	_, err := q.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS runs_quota ON runs (project, archived)`)
	return err
}
