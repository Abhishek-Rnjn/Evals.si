package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

func TestAKSFollowupDependencyRestore(t *testing.T) {
	if os.Getenv("EVALSI_AKS_FOLLOWUP_RECOVERY") != "1" {
		t.Skip("explicit disposable-namespace opt-in required")
	}
	t.Run("clickhouse", func(t *testing.T) {
		client := &http.Client{Timeout: time.Minute}
		query := func(sql string) []byte {
			t.Helper()
			response, err := client.Post(os.Getenv("EVALSI_TEST_CLICKHOUSE_URL"), "text/plain", bytes.NewBufferString(sql))
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil || response.StatusCode != 200 {
				t.Fatalf("query %s: HTTP %d: %s: %v", sql, response.StatusCode, body, err)
			}
			return body
		}
		database := fmt.Sprintf("followup_backup_%d", time.Now().UnixNano())
		query("CREATE DATABASE " + database)
		query("CREATE TABLE " + database + ".records (id UInt64, payload String) ENGINE=MergeTree ORDER BY id")
		query("INSERT INTO " + database + ".records SELECT number, concat('synthetic-', toString(number)) FROM numbers(100)")
		fingerprint := "SELECT count(),sum(id),groupBitXor(cityHash64(toString(id),payload)) FROM " + database + ".records FORMAT JSONEachRow"
		before := query(fingerprint)
		backup := fmt.Sprintf("Disk('backups','%s.zip')", database)
		query("BACKUP DATABASE " + database + " TO " + backup)
		query("DROP DATABASE " + database)
		start := time.Now()
		query("RESTORE DATABASE " + database + " FROM " + backup)
		after := query(fingerprint)
		if !bytes.Equal(before, after) {
			t.Fatalf("restored fingerprint differs: before=%s after=%s", before, after)
		}
		t.Logf("100 synthetic rows restored after source deletion; fingerprint=%s restore_seconds=%.3f", before, time.Since(start).Seconds())
	})
	t.Run("jetstream", func(t *testing.T) {
		nc, err := nats.Connect(os.Getenv("EVALSI_TEST_NATS_URL"))
		if err != nil {
			t.Fatal(err)
		}
		defer nc.Close()
		js, err := nc.JetStream()
		if err != nil {
			t.Fatal(err)
		}
		stream := fmt.Sprintf("FOLLOWUP_BACKUP_%d", time.Now().UnixNano())
		subject := "followup.backup." + stream
		_, err = js.AddStream(&nats.StreamConfig{Name: stream, Subjects: []string{subject}, Storage: nats.FileStorage})
		if err != nil {
			t.Fatal(err)
		}
		for i := range 50 {
			if _, err := js.Publish(subject, fmt.Appendf(nil, "synthetic-%d", i)); err != nil {
				t.Fatal(err)
			}
		}
		sub, err := js.PullSubscribe(subject, "pending", nats.ManualAck(), nats.AckWait(5*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		messages, err := sub.Fetch(10, nats.MaxWait(10*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		for _, message := range messages[:7] {
			if err := message.AckSync(); err != nil {
				t.Fatal(err)
			}
		}
		before, err := js.ConsumerInfo(stream, "pending")
		if err != nil {
			t.Fatal(err)
		}
		var lock sync.Mutex
		var snapshot bytes.Buffer
		done := make(chan struct{})
		deliver := nats.NewInbox()
		snapshotSub, err := nc.Subscribe(deliver, func(message *nats.Msg) {
			if len(message.Data) == 0 {
				close(done)
				return
			}
			lock.Lock()
			_, _ = snapshot.Write(message.Data)
			lock.Unlock()
			if err := message.Respond(nil); err != nil {
				t.Errorf("snapshot acknowledgment: %v", err)
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		defer snapshotSub.Unsubscribe()
		if err := nc.Flush(); err != nil {
			t.Fatal(err)
		}
		request, _ := json.Marshal(map[string]any{"deliver_subject": deliver, "chunk_size": 1024, "no_consumers": false, "check_msgs": true})
		response, err := nc.Request("$JS.API.STREAM.SNAPSHOT."+stream, request, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		var metadata map[string]json.RawMessage
		if err := json.Unmarshal(response.Data, &metadata); err != nil || metadata["error"] != nil {
			t.Fatalf("snapshot refused: %s: %v", response.Data, err)
		}
		select {
		case <-done:
		case <-time.After(time.Minute):
			t.Fatal("snapshot did not complete")
		}
		lock.Lock()
		archive := bytes.Clone(snapshot.Bytes())
		lock.Unlock()
		if len(archive) == 0 {
			t.Fatal("empty snapshot")
		}
		if err := js.DeleteStream(stream); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		request, err = json.Marshal(map[string]json.RawMessage{"config": metadata["config"], "state": metadata["state"]})
		if err != nil {
			t.Fatal(err)
		}
		response, err = nc.Request("$JS.API.STREAM.RESTORE."+stream, request, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		var restore struct {
			Deliver string          `json:"deliver_subject"`
			Error   json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(response.Data, &restore); err != nil || restore.Deliver == "" || restore.Error != nil {
			t.Fatalf("restore refused: %s: %v", response.Data, err)
		}
		for offset := 0; offset < len(archive); offset += 1024 {
			end := min(offset+1024, len(archive))
			if _, err := nc.Request(restore.Deliver, archive[offset:end], time.Minute); err != nil {
				t.Fatal(err)
			}
		}
		response, err = nc.Request(restore.Deliver, nil, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		var result struct {
			Error json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(response.Data, &result); err != nil || result.Error != nil {
			t.Fatalf("restore failed: %s: %v", response.Data, err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		info, err := js.StreamInfo(stream, nats.Context(ctx))
		if err != nil || info.State.Msgs != 50 {
			t.Fatalf("restored stream: %+v: %v", info, err)
		}
		after, err := js.ConsumerInfo(stream, "pending")
		if err != nil {
			t.Fatal(err)
		}
		if after.NumAckPending != 3 || after.NumPending != before.NumPending ||
			after.AckFloor.Consumer != before.AckFloor.Consumer || after.AckFloor.Stream != before.AckFloor.Stream ||
			after.Delivered.Consumer != before.Delivered.Consumer || after.Delivered.Stream != before.Delivered.Stream {
			t.Fatalf("consumer state changed: before=%+v after=%+v", before, after)
		}
		t.Logf("ack-floor last-activity timestamp available: before=%t after=%t; timestamps are not asserted as durable sequence state", before.AckFloor.Last != nil, after.AckFloor.Last != nil)
		for sequence := uint64(1); sequence <= 50; sequence++ {
			message, err := js.GetMsg(stream, sequence)
			if err != nil || string(message.Data) != fmt.Sprintf("synthetic-%d", sequence-1) {
				t.Fatalf("restored message %d differs: %v", sequence, err)
			}
		}
		t.Logf("50 exact messages, consumer ack floor and 3 pending acks restored; snapshot_sha256=%x restore_seconds=%.3f", sha256.Sum256(archive), time.Since(start).Seconds())
	})
}
