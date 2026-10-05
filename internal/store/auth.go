package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/auth"
	"github.com/abhishek-rnjn/evals.si/internal/authz"
)

// ErrExists is returned when creating something whose name is taken.
var ErrExists = authz.ErrExists

const authSchema = `
CREATE TABLE IF NOT EXISTS projects (
  name TEXT PRIMARY KEY, description TEXT NOT NULL, created_at INTEGER NOT NULL, created_by TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS api_keys (
  name TEXT PRIMARY KEY, hash TEXT NOT NULL UNIQUE, prefix TEXT NOT NULL,
  roles TEXT NOT NULL, labels TEXT NOT NULL,
  created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL, last_used_at INTEGER NOT NULL,
  created_by TEXT NOT NULL, revoked INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS roles (
  project TEXT NOT NULL, name TEXT NOT NULL, def TEXT NOT NULL,
  PRIMARY KEY (project, name)
);
CREATE TABLE IF NOT EXISTS bindings (
  project TEXT NOT NULL, role TEXT NOT NULL, subject TEXT NOT NULL,
  created_at INTEGER NOT NULL, created_by TEXT NOT NULL,
  PRIMARY KEY (project, role, subject)
);
CREATE TABLE IF NOT EXISTS audit (
  id INTEGER PRIMARY KEY AUTOINCREMENT, time_ns INTEGER NOT NULL, project TEXT NOT NULL,
  action TEXT NOT NULL, allowed INTEGER NOT NULL, event BLOB NOT NULL
);
CREATE INDEX IF NOT EXISTS audit_by_project ON audit (project, id DESC);
`

func isUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func nanos(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func fromNanos(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}

// --- projects ---

// CreateProject stores a project; ErrExists when the name is taken.
func (s *Store) CreateProject(ctx context.Context, p authz.Project) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO projects (name, description, created_at, created_by) VALUES (?, ?, ?, ?)`,
		p.Name, p.Description, nanos(p.CreatedAt), p.CreatedBy)
	if isUnique(err) {
		return ErrExists
	}
	return err
}

// AuthProjects lists stored projects.
func (s *Store) AuthProjects(ctx context.Context) ([]authz.Project, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, description, created_at, created_by FROM projects ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []authz.Project
	for rows.Next() {
		var p authz.Project
		var at int64
		if err := rows.Scan(&p.Name, &p.Description, &at, &p.CreatedBy); err != nil {
			return nil, err
		}
		p.CreatedAt = fromNanos(at)
		out = append(out, p)
	}
	return out, rows.Err()
}

// --- roles ---

// PutRole creates (create=true, ErrExists if taken) or replaces (ErrNotFound if absent) a role.
func (s *Store) PutRole(ctx context.Context, r authz.Role, create bool) error {
	def, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if create {
		_, err = s.db.ExecContext(ctx, `INSERT INTO roles (project, name, def) VALUES (?, ?, ?)`, r.Project, r.Name, string(def))
		if isUnique(err) {
			return ErrExists
		}
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE roles SET def = ? WHERE project = ? AND name = ?`, string(def), r.Project, r.Name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteRole removes a stored role.
func (s *Store) DeleteRole(ctx context.Context, project, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM roles WHERE project = ? AND name = ?`, project, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// AuthRoles lists stored roles.
func (s *Store) AuthRoles(ctx context.Context) ([]authz.Role, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT def FROM roles ORDER BY project, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []authz.Role
	for rows.Next() {
		var def string
		if err := rows.Scan(&def); err != nil {
			return nil, err
		}
		var r authz.Role
		if err := json.Unmarshal([]byte(def), &r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// --- bindings ---

// CreateBinding stores a binding; ErrExists if it is already there.
func (s *Store) CreateBinding(ctx context.Context, b authz.Binding) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO bindings (project, role, subject, created_at, created_by) VALUES (?, ?, ?, ?, ?)`,
		b.Project, b.Role, b.Subject, nanos(b.CreatedAt), b.CreatedBy)
	if isUnique(err) {
		return ErrExists
	}
	return err
}

// DeleteBinding removes a stored binding.
func (s *Store) DeleteBinding(ctx context.Context, b authz.Binding) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM bindings WHERE project = ? AND role = ? AND subject = ?`, b.Project, b.Role, b.Subject)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// AuthBindings lists stored bindings.
func (s *Store) AuthBindings(ctx context.Context) ([]authz.Binding, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT project, role, subject, created_at, created_by FROM bindings ORDER BY project, role, subject`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []authz.Binding
	for rows.Next() {
		var b authz.Binding
		var at int64
		if err := rows.Scan(&b.Project, &b.Role, &b.Subject, &at, &b.CreatedBy); err != nil {
			return nil, err
		}
		b.CreatedAt = fromNanos(at)
		out = append(out, b)
	}
	return out, rows.Err()
}

// --- API keys ---

// CreateAPIKey stores a key; ErrExists when the name is taken.
func (s *Store) CreateAPIKey(ctx context.Context, k authz.KeyRecord) error {
	roles, _ := json.Marshal(k.Roles)
	labels, _ := json.Marshal(k.Labels)
	_, err := s.db.ExecContext(ctx, `INSERT INTO api_keys
		(name, hash, prefix, roles, labels, created_at, expires_at, last_used_at, created_by, revoked)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?, 0)`,
		k.Name, k.Hash, k.Prefix, string(roles), string(labels), nanos(k.CreatedAt), nanos(k.ExpiresAt), k.CreatedBy)
	if isUnique(err) {
		return ErrExists
	}
	return err
}

const keyColumns = `name, hash, prefix, roles, labels, created_at, expires_at, last_used_at, created_by, revoked`

func scanKey(scan func(...any) error) (authz.KeyRecord, error) {
	var k authz.KeyRecord
	var roles, labels string
	var created, expires, used int64
	if err := scan(&k.Name, &k.Hash, &k.Prefix, &roles, &labels, &created, &expires, &used, &k.CreatedBy, &k.Revoked); err != nil {
		return k, err
	}
	if err := json.Unmarshal([]byte(roles), &k.Roles); err != nil {
		return k, err
	}
	if err := json.Unmarshal([]byte(labels), &k.Labels); err != nil {
		return k, err
	}
	k.CreatedAt, k.ExpiresAt, k.LastUsedAt = fromNanos(created), fromNanos(expires), fromNanos(used)
	return k, nil
}

// APIKeys lists stored keys, revoked ones included.
func (s *Store) APIKeys(ctx context.Context) ([]authz.KeyRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+keyColumns+` FROM api_keys ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []authz.KeyRecord
	for rows.Next() {
		k, err := scanKey(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// APIKey returns a stored key by name.
func (s *Store) APIKey(ctx context.Context, name string) (authz.KeyRecord, error) {
	k, err := scanKey(s.db.QueryRowContext(ctx, `SELECT `+keyColumns+` FROM api_keys WHERE name = ?`, name).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return k, ErrNotFound
	}
	return k, err
}

// KeyByHash implements auth.KeyStore.
func (s *Store) KeyByHash(ctx context.Context, hash string) (*auth.StoredKey, error) {
	k, err := scanKey(s.db.QueryRowContext(ctx, `SELECT `+keyColumns+` FROM api_keys WHERE hash = ?`, hash).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &auth.StoredKey{Name: k.Name, Roles: k.Roles, Labels: k.Labels, ExpiresAt: k.ExpiresAt, Revoked: k.Revoked}, nil
}

// TouchKey implements auth.KeyStore.
func (s *Store) TouchKey(ctx context.Context, name string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE api_keys SET last_used_at = ? WHERE name = ?`, at.UnixNano(), name)
	return err
}

// RevokeAPIKey marks a key revoked; it stays listed for the record.
func (s *Store) RevokeAPIKey(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE api_keys SET revoked = 1 WHERE name = ?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// --- audit ---

// AppendAudit adds an event and returns its id.
func (s *Store) AppendAudit(ctx context.Context, ev *evalsiv1alpha1.AuditEvent) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO audit (time_ns, project, action, allowed, event) VALUES (?, ?, ?, ?, ?)`,
		ev.GetTime().AsTime().UnixNano(), ev.GetProject(), ev.GetAction(), ev.GetAllowed(), marshal(ev))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListAudit returns events newest first. pageToken is the id to continue below.
func (s *Store) ListAudit(ctx context.Context, f authz.AuditQuery, pageSize int, pageToken string) ([]*evalsiv1alpha1.AuditEvent, string, error) {
	before := int64(1<<63 - 1)
	if pageToken != "" {
		var err error
		if before, err = strconv.ParseInt(pageToken, 10, 64); err != nil {
			return nil, "", errors.New("store: bad page token")
		}
	}
	where, args := projectFilter("project", f.Projects)
	q := `SELECT id, event FROM audit WHERE id < ? AND ` + where + ` AND (? = '' OR action = ?) AND (? = 0 OR allowed = 0) ORDER BY id DESC LIMIT ?`
	all := append([]any{before}, args...)
	all = append(all, f.Action, f.Action, f.DeniedOnly, pageSize+1)
	rows, err := s.db.QueryContext(ctx, q, all...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var out []*evalsiv1alpha1.AuditEvent
	for rows.Next() {
		var id int64
		var blob []byte
		if err := rows.Scan(&id, &blob); err != nil {
			return nil, "", err
		}
		ev := &evalsiv1alpha1.AuditEvent{}
		if err := proto.Unmarshal(blob, ev); err != nil {
			return nil, "", err
		}
		ev.Id = id
		out = append(out, ev)
	}
	next := ""
	if len(out) > pageSize {
		out = out[:pageSize]
		next = strconv.FormatInt(out[len(out)-1].GetId(), 10)
	}
	return out, next, rows.Err()
}

// DeleteAuditBefore removes events older than t.
func (s *Store) DeleteAuditBefore(ctx context.Context, t time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM audit WHERE time_ns < ?`, t.UnixNano())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// projectFilter is a SQL condition limiting a column to projects; nil means every project.
func projectFilter(column string, projects []string) (string, []any) {
	if projects == nil {
		return "1 = 1", nil
	}
	if len(projects) == 0 {
		return "1 = 0", nil
	}
	args := make([]any, len(projects))
	for i, p := range projects {
		args[i] = p
	}
	return column + " IN (" + strings.TrimSuffix(strings.Repeat("?, ", len(projects)), ", ") + ")", args
}
