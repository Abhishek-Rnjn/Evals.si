package watch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"slices"

	"google.golang.org/protobuf/proto"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/ingest"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

type pendingPayload struct {
	Acceptance [16]byte
	Record     []byte
	Info       ingest.TraceInfo
	Only       []string
	Stream     string
}

func init() {
	// OTLP attributes can contain nested maps and arrays. Gob keeps int64 and
	// float64 distinct, including integers beyond JSON's exact double range.
	gob.Register(map[string]any{})
	gob.Register([]any{})
	gob.Register(ingest.Attrs{})
	gob.Register([]string{})
}

func encodePending(p pendingPayload) ([]byte, error) {
	var b bytes.Buffer
	err := gob.NewEncoder(&b).Encode(p)
	return b.Bytes(), err
}

func decodePending(blob []byte, p *pendingPayload) error {
	return gob.NewDecoder(bytes.NewReader(blob)).Decode(p)
}

func (it *item) initPending() store.WatchPending {
	if it.summary == nil {
		it.summary = Summary(it.record, it.info)
	}
	recHash := store.RecordHash(it.record)
	it.pend = store.WatchPending{
		Project:      it.info.Project,
		TraceID:      it.record.GetId(),
		Stream:       it.stream,
		Payload:      it.payload,
		RecordHash:   recHash,
		SnapshotHash: store.SnapshotHash(it.summary, it.record),
		StartNS:      it.summary.GetStartTime().AsTime().UnixNano(),
	}
	return it.pend
}

func (it item) pending() store.WatchPending {
	if it.pend.Project != "" || it.pend.TraceID != "" {
		it.pend.Payload = it.payload
		return it.pend
	}
	return (&it).initPending()
}

// recoverPending replays bounded batches, only on the elected policy engine.
// Delivery is at least once: a crash after an evaluator call can repeat it.
func (e *Engine) recoverPending(ctx context.Context) {
	if e.follower.Load() {
		return
	}
	limit := max(e.opts.BatchSize, 500)
	rows, err := e.store.ListWatchPending(ctx, limit, e.recoveryCursor)
	if err != nil {
		e.log.Error("reading pending online traces", "err", err)
		return
	}
	if len(rows) < limit {
		e.recoveryCursor = store.WatchPending{}
	} else {
		e.recoveryCursor = rows[len(rows)-1]
	}
	var batch []item
	for _, row := range rows {
		var p pendingPayload
		record := &evalsiv1alpha1.Record{}
		if err := decodePending(row.Payload, &p); err != nil {
			e.log.Error("decoding pending online trace", "trace", row.TraceID, "err", err)
			continue
		}
		if err := proto.Unmarshal(p.Record, record); err != nil {
			e.log.Error("decoding pending online record", "trace", row.TraceID, "err", err)
			continue
		}
		batch = append(batch, item{record: record, info: p.Info, only: p.Only, stream: row.Stream, payload: row.Payload})
	}
	if !e.follower.Load() {
		e.process(ctx, batch)
	}
}

// Preserve independent source/policy work even when they share trace IDs.
func workStream(info ingest.TraceInfo, only []string) string {
	name := info.Labels["source"]
	if name == "" && len(only) == 0 {
		return ""
	}
	policies := slices.Clone(only)
	slices.Sort(policies)
	policies = slices.Compact(policies)
	blob, _ := json.Marshal(struct {
		Source   string
		Policies []string
	}{name, policies})
	return fmt.Sprintf("%x", sha256.Sum256(blob))
}
