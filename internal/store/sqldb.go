package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
)

// dialect is the SQL database behind the store. Statements are written once,
// for SQLite (`?` placeholders, BLOB and INTEGER columns, standard ON CONFLICT
// upserts); conn rewrites them for PostgreSQL.
type dialect int

const (
	sqliteDialect dialect = iota
	postgresDialect
)

// rebind turns `?` placeholders into PostgreSQL's `$n`. The store's
// statements never contain a literal question mark.
func (d dialect) rebind(q string) string {
	if d != postgresDialect || !strings.Contains(q, "?") {
		return q
	}
	var b strings.Builder
	n := 0
	for _, r := range q {
		if r == '?' {
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// args adapts arguments: PostgreSQL does not store a bool in an integer
// column the way SQLite does.
func (d dialect) args(args []any) []any {
	if d != postgresDialect {
		return args
	}
	for i, a := range args {
		if b, ok := a.(bool); ok {
			if b {
				args[i] = int64(1)
			} else {
				args[i] = int64(0)
			}
		}
	}
	return args
}

// ddl turns the SQLite schema into PostgreSQL's.
func (d dialect) ddl(schema string) string {
	if d != postgresDialect {
		return schema
	}
	// In order: the serial key before plain INTEGER columns.
	schema = strings.ReplaceAll(schema, "INTEGER PRIMARY KEY AUTOINCREMENT", "BIGSERIAL PRIMARY KEY")
	schema = strings.ReplaceAll(schema, " INTEGER", " BIGINT")
	return strings.ReplaceAll(schema, " BLOB", " BYTEA")
}

// conn is a database handle that speaks the store's SQL in either dialect.
type conn struct {
	db *sql.DB
	d  dialect
}

func (c *conn) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return c.db.ExecContext(ctx, c.d.rebind(q), c.d.args(args)...)
}

func (c *conn) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return c.db.QueryContext(ctx, c.d.rebind(q), c.d.args(args)...)
}

func (c *conn) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	return c.db.QueryRowContext(ctx, c.d.rebind(q), c.d.args(args)...)
}

func (c *conn) BeginTx(ctx context.Context, opts *sql.TxOptions) (*txn, error) {
	tx, err := c.db.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &txn{tx: tx, d: c.d}, nil
}

func (c *conn) Close() error { return c.db.Close() }

type txn struct {
	tx *sql.Tx
	d  dialect
}

func (t *txn) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return t.tx.ExecContext(ctx, t.d.rebind(q), t.d.args(args)...)
}

func (t *txn) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	return t.tx.QueryRowContext(ctx, t.d.rebind(q), t.d.args(args)...)
}

func (t *txn) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return t.tx.QueryContext(ctx, t.d.rebind(q), t.d.args(args)...)
}

func (t *txn) PrepareContext(ctx context.Context, q string) (*sql.Stmt, error) {
	return t.tx.PrepareContext(ctx, t.d.rebind(q))
}

func (t *txn) Commit() error   { return t.tx.Commit() }
func (t *txn) Rollback() error { return t.tx.Rollback() }

// tenantTables are the project-scoped tables. Each carries a reserved
// tenant_id column (empty in a single-tenant install), so a hosted
// multi-tenant offering can put a tenant boundary above projects later
// without rewriting stored rows (decision 0004).
var tenantTables = []string{"projects", "api_keys", "roles", "bindings", "audit", "runs", "traces", "trace_results", "policies", "annotation_queues", "annotation_items", "annotations", "annotation_claims", "guardrails"}

type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// addedColumns are columns added to tables after their first release; each
// is added to a database that lacks it.
var addedColumns = []struct{ table, column string }{
	{"webhook_deliveries", "trace_id"},
	{"webhook_deliveries", "policy"},
}

// addTenantColumns adds tenant_id to every project-scoped table that lacks
// it, and the other added columns.
func addTenantColumns(ctx context.Context, q querier, d dialect) error {
	for _, table := range tenantTables {
		if err := addColumn(ctx, q, d, table, "tenant_id"); err != nil {
			return err
		}
	}
	for _, c := range addedColumns {
		if err := addColumn(ctx, q, d, c.table, c.column); err != nil {
			return err
		}
	}
	return nil
}

// addColumn adds a text column, empty by default, when the table lacks it.
func addColumn(ctx context.Context, q querier, d dialect, table, column string) error {
	if d == postgresDialect {
		if _, err := q.ExecContext(ctx, `ALTER TABLE `+table+` ADD COLUMN IF NOT EXISTS `+column+` TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("adding %s to %s: %w", column, table, err)
		}
		return nil
	}
	var n int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		if _, err := q.ExecContext(ctx, `ALTER TABLE `+table+` ADD COLUMN `+column+` TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("adding %s to %s: %w", column, table, err)
		}
	}
	return nil
}
