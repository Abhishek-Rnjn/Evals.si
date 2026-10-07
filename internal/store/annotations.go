package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	"google.golang.org/protobuf/proto"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

// annotationSchema holds human annotation queues, their items and answers.
// Annotators claim items with a lease (annotation_claims); an item is offered
// only while its answers plus others' live claims fall short of what it
// needs, so annotators working at once get different items.
const annotationSchema = `
CREATE TABLE IF NOT EXISTS annotation_queues (
  project TEXT NOT NULL, name TEXT NOT NULL, body BLOB NOT NULL, created_ns INTEGER NOT NULL,
  PRIMARY KEY (project, name)
);
CREATE TABLE IF NOT EXISTS annotation_items (
  id TEXT PRIMARY KEY, project TEXT NOT NULL, queue TEXT NOT NULL, body BLOB NOT NULL,
  annotations INTEGER NOT NULL DEFAULT 0, done INTEGER NOT NULL DEFAULT 0,
  created_ns INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS annotation_items_by_queue ON annotation_items (project, queue, done, created_ns);
CREATE TABLE IF NOT EXISTS annotations (
  item_id TEXT NOT NULL, annotator TEXT NOT NULL, project TEXT NOT NULL, queue TEXT NOT NULL,
  body BLOB NOT NULL, skipped INTEGER NOT NULL DEFAULT 0, created_ns INTEGER NOT NULL,
  PRIMARY KEY (item_id, annotator)
);
CREATE TABLE IF NOT EXISTS annotation_claims (
  item_id TEXT NOT NULL, annotator TEXT NOT NULL, project TEXT NOT NULL, queue TEXT NOT NULL,
  until_ns INTEGER NOT NULL,
  PRIMARY KEY (item_id, annotator)
);
CREATE INDEX IF NOT EXISTS annotations_by_queue ON annotations (project, queue, created_ns);
`

// CreateQueue stores a new annotation queue.
func (s *Store) CreateQueue(ctx context.Context, q *evalsiv1alpha1.AnnotationQueue) error {
	body, err := proto.Marshal(q)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO annotation_queues (project, name, body, created_ns) VALUES (?, ?, ?, ?) ON CONFLICT (project, name) DO NOTHING`,
		q.GetProject(), q.GetName(), body, time.Now().UnixNano())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrExists
	}
	return nil
}

// GetQueue returns a queue.
func (s *Store) GetQueue(ctx context.Context, project, name string) (*evalsiv1alpha1.AnnotationQueue, error) {
	var body []byte
	err := s.db.QueryRowContext(ctx, `SELECT body FROM annotation_queues WHERE project = ? AND name = ?`, project, name).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	q := &evalsiv1alpha1.AnnotationQueue{}
	return q, proto.Unmarshal(body, q)
}

// ListQueues returns a project's queues, by name ("" lists every project's).
func (s *Store) ListQueues(ctx context.Context, project string) ([]*evalsiv1alpha1.AnnotationQueue, error) {
	q, args := `SELECT body FROM annotation_queues ORDER BY project, name`, []any{}
	if project != "" {
		q, args = `SELECT body FROM annotation_queues WHERE project = ? ORDER BY name`, []any{project}
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*evalsiv1alpha1.AnnotationQueue
	for rows.Next() {
		var body []byte
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		m := &evalsiv1alpha1.AnnotationQueue{}
		if err := proto.Unmarshal(body, m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// DeleteQueue removes a queue with its items and annotations.
func (s *Store) DeleteQueue(ctx context.Context, project, name string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `DELETE FROM annotation_queues WHERE project = ? AND name = ?`, project, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	for _, q := range []string{
		`DELETE FROM annotations WHERE project = ? AND queue = ?`,
		`DELETE FROM annotation_claims WHERE project = ? AND queue = ?`,
		`DELETE FROM annotation_items WHERE project = ? AND queue = ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, project, name); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func newItemID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// AddItems appends items to a queue, in order, giving them ids.
func (s *Store) AddItems(ctx context.Context, project, queue string, items []*evalsiv1alpha1.AnnotationItem) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	base := time.Now().UnixNano()
	for i, it := range items {
		if it.GetId() == "" {
			it.Id = newItemID()
		}
		it.Queue = queue
		body, err := proto.Marshal(it)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO annotation_items (id, project, queue, body, created_ns) VALUES (?, ?, ?, ?, ?)`,
			it.GetId(), project, queue, body, base+int64(i)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) item(ctx context.Context, q querier, id string) (*evalsiv1alpha1.AnnotationItem, error) {
	var body []byte
	var annotations int64
	var done bool
	err := q.QueryRowContext(ctx, `SELECT body, annotations, done FROM annotation_items WHERE id = ?`, id).Scan(&body, &annotations, &done)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	it := &evalsiv1alpha1.AnnotationItem{}
	if err := proto.Unmarshal(body, it); err != nil {
		return nil, err
	}
	it.Annotations, it.Done = int32(annotations), done
	return it, nil
}

// GetItem returns an item of a queue.
func (s *Store) GetItem(ctx context.Context, project, queue, id string) (*evalsiv1alpha1.AnnotationItem, error) {
	it, err := s.item(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	var p string
	if err := s.db.QueryRowContext(ctx, `SELECT project FROM annotation_items WHERE id = ?`, id).Scan(&p); err != nil || p != project || it.GetQueue() != queue {
		return nil, ErrNotFound
	}
	return it, nil
}

// ClaimItem leases the caller the next item it has not answered whose
// answers plus other annotators' live claims are fewer than need (an item
// it already holds comes first). Under concurrent claims on Postgres an
// item can briefly be over-claimed, which costs at most an extra answer.
func (s *Store) ClaimItem(ctx context.Context, project, queue, annotator string, need int, lease time.Duration, now time.Time) (*evalsiv1alpha1.AnnotationItem, int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM annotation_claims WHERE project = ? AND queue = ? AND until_ns < ?`, project, queue, now.UnixNano()); err != nil {
		return nil, 0, err
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT i.id FROM annotation_items i
		 WHERE i.project = ? AND i.queue = ? AND i.done = 0
		   AND i.id NOT IN (SELECT item_id FROM annotations WHERE project = ? AND queue = ? AND annotator = ?)
		   AND (EXISTS (SELECT 1 FROM annotation_claims c WHERE c.item_id = i.id AND c.annotator = ?)
		        OR i.annotations + (SELECT COUNT(*) FROM annotation_claims c WHERE c.item_id = i.id) < ?)
		 ORDER BY CASE WHEN EXISTS (SELECT 1 FROM annotation_claims c WHERE c.item_id = i.id AND c.annotator = ?) THEN 0 ELSE 1 END,
		   i.created_ns, i.id`,
		project, queue, project, queue, annotator, annotator, need, annotator)
	if err != nil {
		return nil, 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return nil, 0, err
	}
	if len(ids) == 0 {
		return nil, 0, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO annotation_claims (item_id, annotator, project, queue, until_ns) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (item_id, annotator) DO UPDATE SET until_ns = excluded.until_ns`,
		ids[0], annotator, project, queue, now.Add(lease).UnixNano()); err != nil {
		return nil, 0, err
	}
	it, err := s.item(ctx, tx, ids[0])
	if err != nil {
		return nil, 0, err
	}
	return it, int64(len(ids)), tx.Commit()
}

// SaveAnnotation records (or replaces) an annotator's answer to an item,
// recounts the item's answers, marks it done at need, and releases the
// annotator's claim.
func (s *Store) SaveAnnotation(ctx context.Context, project string, a *evalsiv1alpha1.Annotation, need int) (*evalsiv1alpha1.AnnotationItem, error) {
	body, err := proto.Marshal(a)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO annotations (item_id, annotator, project, queue, body, skipped, created_ns) VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (item_id, annotator) DO UPDATE SET body = excluded.body, skipped = excluded.skipped, created_ns = excluded.created_ns`,
		a.GetItemId(), a.GetAnnotator(), project, a.GetQueue(), body, a.GetSkipped(), a.GetCreatedAt().AsTime().UnixNano()); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE annotation_items SET annotations = (SELECT COUNT(*) FROM annotations WHERE item_id = ? AND skipped = 0) WHERE id = ?`,
		a.GetItemId(), a.GetItemId()); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM annotation_claims WHERE item_id = ? AND annotator = ?`, a.GetItemId(), a.GetAnnotator()); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE annotation_items SET done = CASE WHEN annotations >= ? THEN 1 ELSE 0 END WHERE id = ?`, need, a.GetItemId()); err != nil {
		return nil, err
	}
	it, err := s.item(ctx, tx, a.GetItemId())
	if err != nil {
		return nil, err
	}
	return it, tx.Commit()
}

// QueueItems returns a queue's items, oldest first (offset and limit page
// them; limit 0 means all).
func (s *Store) QueueItems(ctx context.Context, project, queue string, offset, limit int) ([]*evalsiv1alpha1.AnnotationItem, error) {
	q := `SELECT id FROM annotation_items WHERE project = ? AND queue = ? ORDER BY created_ns, id`
	args := []any{project, queue}
	if limit > 0 {
		q += ` LIMIT ? OFFSET ?`
		args = append(args, limit, offset)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	out := make([]*evalsiv1alpha1.AnnotationItem, 0, len(ids))
	for _, id := range ids {
		it, err := s.item(ctx, s.db, id)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, nil
}

// QueueAnnotations returns a queue's annotations, oldest first (offset and
// limit page them; limit 0 means all).
func (s *Store) QueueAnnotations(ctx context.Context, project, queue string, offset, limit int) ([]*evalsiv1alpha1.Annotation, error) {
	q := `SELECT body FROM annotations WHERE project = ? AND queue = ? ORDER BY created_ns, item_id, annotator`
	args := []any{project, queue}
	if limit > 0 {
		q += ` LIMIT ? OFFSET ?`
		args = append(args, limit, offset)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*evalsiv1alpha1.Annotation
	for rows.Next() {
		var body []byte
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		a := &evalsiv1alpha1.Annotation{}
		if err := proto.Unmarshal(body, a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
