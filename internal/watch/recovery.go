package watch

import (
	"context"
	"encoding/json"

	"google.golang.org/protobuf/proto"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/ingest"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

type pendingPayload struct {
	Record []byte
	Info   ingest.TraceInfo
	Only   []string
}

func (it item) pending() store.WatchPending {
	return store.WatchPending{Project: it.info.Project, TraceID: it.record.GetId(), Payload: it.payload, StartNS: Summary(it.record, it.info).GetStartTime().AsTime().UnixNano()}
}

// recoverPending replays bounded batches, only on the elected policy engine.
// Delivery is at least once: a crash after an evaluator call can repeat it.
func (e *Engine) recoverPending(ctx context.Context) {
	if e.follower.Load() {
		return
	}
	rows, err := e.store.ListWatchPending(ctx, e.opts.BatchSize, e.recoveryCursor)
	if err != nil {
		e.log.Error("reading pending online traces", "err", err)
		return
	}
	if len(rows) < e.opts.BatchSize {
		e.recoveryCursor = store.WatchPending{}
	} else {
		e.recoveryCursor = rows[len(rows)-1]
	}
	var batch []item
	for _, row := range rows {
		var p pendingPayload
		record := &evalsiv1alpha1.Record{}
		if err := json.Unmarshal(row.Payload, &p); err != nil {
			e.log.Error("decoding pending online trace", "trace", row.TraceID, "err", err)
			continue
		}
		if err := proto.Unmarshal(p.Record, record); err != nil {
			e.log.Error("decoding pending online record", "trace", row.TraceID, "err", err)
			continue
		}
		batch = append(batch, item{record: record, info: p.Info, only: p.Only, payload: row.Payload})
	}
	if !e.follower.Load() {
		e.process(ctx, batch)
	}
}
