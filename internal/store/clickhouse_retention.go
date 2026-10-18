package store

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"time"
)

const retentionBatchSize = 500

func expireWatchMetadata(ctx context.Context, q querier, t time.Time) error {
	if _, err := q.ExecContext(ctx, `DELETE FROM watch_pending WHERE start_ns<?`, t.UnixNano()); err != nil {
		return err
	}
	_, err := q.ExecContext(ctx, `UPDATE watch_trace_heads SET deleted=1,published=0 WHERE start_ns<? AND deleted=0`, t.UnixNano())
	return err
}

func finishWatchRetention(ctx context.Context, q querier, t time.Time) error {
	for _, table := range []string{"watch_publications", "watch_score_references"} {
		if _, err := q.ExecContext(ctx, `DELETE FROM `+table+` WHERE start_ns<?`, t.UnixNano()); err != nil {
			return err
		}
	}
	return nil
}

// Commit the retirement decision before external tombstones. A later cleanup
// failure cannot leave a published metadata head pointing at a hidden trace.
// Revision zero records a retryable retention intent, independent of ingestion.
func (s *Store) prepareClickHouseRetention(ctx context.Context, t time.Time) error {
	tx, err := s.beginWatchWrite(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO watch_publications(project,trace_id,revision,start_ns)
 SELECT project,trace_id,0,start_ns FROM watch_trace_heads WHERE start_ns<? AND deleted=0
 ON CONFLICT(project,trace_id,revision) DO UPDATE SET start_ns=CASE WHEN excluded.start_ns<watch_publications.start_ns THEN excluded.start_ns ELSE watch_publications.start_ns END`, t.UnixNano()); err != nil {
		return err
	}
	if err := expireWatchMetadata(ctx, tx, t); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) deleteClickHouseTracesBefore(ctx context.Context, t time.Time, c *clickhouse) (int64, error) {
	for {
		if err := s.prepareClickHouseRetention(ctx, t); err != nil {
			return 0, err
		}
		reserved, tx, err := s.beginVersionedWatchWrite(ctx, 1)
		if err != nil {
			return 0, err
		}
		n, again, err := s.commitClickHouseRetention(reserved, tx, t, c)
		_ = tx.Rollback()
		if err != nil || !again {
			return n, err
		}
	}
}

func (s *Store) commitClickHouseRetention(ctx context.Context, tx *txn, t time.Time, c *clickhouse) (int64, bool, error) {
	// Ingestion may have passed between the retirement commit and this fence.
	// Retire any newly expired head durably before sending external writes.
	var changed int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM watch_trace_heads WHERE start_ns<? AND deleted=0`, t.UnixNano()).Scan(&changed); err != nil {
		return 0, false, err
	}
	if changed > 0 {
		return 0, true, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT project,trace_id,start_ns FROM watch_publications WHERE start_ns<?`, t.UnixNano())
	if err != nil {
		return 0, false, err
	}
	var heads []watchHead
	for rows.Next() {
		var h watchHead
		if err := rows.Scan(&h.Project, &h.TraceID, &h.StartNS); err != nil {
			_ = rows.Close()
			return 0, false, err
		}
		heads = append(heads, h)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, false, err
	}
	if err := rows.Close(); err != nil {
		return 0, false, err
	}
	if err := s.stageClickHouseScoreReferences(ctx, tx, c); err != nil {
		return 0, false, err
	}
	n, err := c.retainTraces(ctx, t, heads)
	if err != nil {
		return n, false, err
	}
	if err := finishWatchRetention(ctx, tx, t); err != nil {
		return n, false, err
	}
	return n, false, tx.Commit()
}

// Only changed references are staged, in bounded request bodies. ClickHouse
// keeps their maximum start time permanently so late expired result INSERTs
// remain collectible after metadata references have been retired.
func (s *Store) stageClickHouseScoreReferences(ctx context.Context, q querier, c *clickhouse) error {
	for {
		rows, err := q.QueryContext(ctx, `SELECT project,trace_id,record_hash,start_ns FROM watch_score_references WHERE dirty=1 ORDER BY project,trace_id,record_hash LIMIT ?`, retentionBatchSize)
		if err != nil {
			return err
		}
		type scoreReference struct {
			Project string `json:"project"`
			TraceID string `json:"trace_id"`
			Hash    string `json:"record_hash"`
			Start   int64  `json:"start_ns"`
		}
		var refs []scoreReference
		var body bytes.Buffer
		enc := json.NewEncoder(&body)
		for rows.Next() {
			var ref scoreReference
			if err := rows.Scan(&ref.Project, &ref.TraceID, &ref.Hash, &ref.Start); err != nil {
				_ = rows.Close()
				return err
			}
			refs = append(refs, ref)
			if err := enc.Encode(ref); err != nil {
				_ = rows.Close()
				return err
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if len(refs) == 0 {
			return nil
		}
		if err := c.exec(ctx, `INSERT INTO trace_score_references FORMAT JSONEachRow`, nil, &body); err != nil {
			return err
		}
		for _, ref := range refs {
			if _, err := q.ExecContext(ctx, `UPDATE watch_score_references SET dirty=0 WHERE project=? AND trace_id=? AND record_hash=? AND start_ns=?`, ref.Project, ref.TraceID, ref.Hash, ref.Start); err != nil {
				return err
			}
		}
	}
}

// Every staged key has the current fencing version; older delayed staging
// requests cannot replace it. Keys and score references never travel in URLs.
func (c *clickhouse) stageRetentionHeads(ctx context.Context, heads []watchHead, version int64) error {
	for offset := 0; offset < len(heads); offset += retentionBatchSize {
		end := min(offset+retentionBatchSize, len(heads))
		var body bytes.Buffer
		enc := json.NewEncoder(&body)
		for _, h := range heads[offset:end] {
			_ = enc.Encode(map[string]any{"project": h.Project, "trace_id": h.TraceID, "start_ns": h.StartNS, "version": version})
		}
		if err := c.exec(ctx, `INSERT INTO trace_retention_requests FORMAT JSONEachRow`, nil, &body); err != nil {
			return err
		}
	}
	return nil
}

func (c *clickhouse) retainTraces(ctx context.Context, t time.Time, pending []watchHead) (int64, error) {
	version := traceWriteVersion(ctx)
	params := chParams{"before": strconv.FormatInt(t.UnixNano(), 10), "version": strconv.FormatInt(version, 10)}
	if err := c.stageRetentionHeads(ctx, pending, version); err != nil {
		return 0, err
	}
	keys := map[[2]string]watchHead{}
	for _, h := range pending {
		keys[[2]string{h.Project, h.TraceID}] = h
	}
	var writes []map[string]any
	var n int64
	err := c.query(ctx, `SELECT project,trace_id,start_ns,service,base64Encode(summary) AS summary,base64Encode(record) AS record,record_hash FROM trace_current
 WHERE start_ns<{before:Int64} OR (project,trace_id) IN (SELECT project,trace_id FROM trace_retention_requests FINAL WHERE version={version:Int64})`, params, func(line []byte) error {
		var row struct {
			Project string `json:"project"`
			TraceID string `json:"trace_id"`
			Start   int64  `json:"start_ns"`
			Service string `json:"service"`
			Summary string `json:"summary"`
			Record  string `json:"record"`
			Hash    string `json:"record_hash"`
		}
		if err := json.Unmarshal(line, &row); err != nil {
			return err
		}
		key := [2]string{row.Project, row.TraceID}
		if row.Start < t.UnixNano() {
			n++
			keys[key] = watchHead{Project: row.Project, TraceID: row.TraceID, StartNS: row.Start}
		} else if _, ok := keys[key]; ok {
			delete(keys, key)
			writes = append(writes, map[string]any{"project": row.Project, "trace_id": row.TraceID, "service": row.Service, "start_ns": row.Start, "summary": row.Summary, "record": row.Record, "record_hash": row.Hash, "version": version, "deleted": 0})
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	for _, h := range keys {
		writes = append(writes, map[string]any{"project": h.Project, "trace_id": h.TraceID, "service": "", "start_ns": h.StartNS, "summary": "", "record": "", "record_hash": "", "version": version, "deleted": 1})
	}
	for offset := 0; offset < len(writes); offset += retentionBatchSize {
		var body bytes.Buffer
		enc := json.NewEncoder(&body)
		for _, row := range writes[offset:min(offset+retentionBatchSize, len(writes))] {
			_ = enc.Encode(row)
		}
		if err := c.exec(ctx, `INSERT INTO traces(project,trace_id,service,start_ns,summary,record,version,record_hash,deleted) SELECT project,trace_id,service,start_ns,base64Decode(summary),base64Decode(record),version,record_hash,deleted FROM input('project String,trace_id String,service String,start_ns Int64,summary String,record String,version Int64,record_hash String,deleted UInt8') FORMAT JSONEachRow`, nil, &body); err != nil {
			return 0, err
		}
	}
	// Keep compact tombstones across monthly partitions; reclaim only versions
	// covered by this fence, so a delayed mutation cannot delete newer backfills.
	if err := c.exec(ctx, `DELETE FROM traces WHERE deleted=0 AND version<={version:Int64} AND (start_ns<{before:Int64} OR (project,trace_id) IN (SELECT project,trace_id FROM traces FINAL WHERE deleted=1 AND start_ns<{before:Int64}))`, params, nil); err != nil {
		return 0, err
	}
	if err := c.exec(ctx, `DELETE FROM trace_results WHERE version<={version:Int64} AND
 ((project,trace_id) IN (SELECT project,trace_id FROM traces FINAL WHERE deleted=1 AND start_ns<{before:Int64})
 OR (project,trace_id,record_hash) IN (SELECT project,trace_id,record_hash FROM trace_score_references FINAL WHERE start_ns<{before:Int64}))
 AND (project,trace_id,record_hash) NOT IN (SELECT project,trace_id,record_hash FROM trace_score_references FINAL WHERE start_ns>={before:Int64})`, params, nil); err != nil {
		return 0, err
	}
	return n, nil
}
