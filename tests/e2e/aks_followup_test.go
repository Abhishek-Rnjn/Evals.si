package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/abhishek-rnjn/evals.si/internal/config"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

// Reuses the unchanged load checks and thresholds with explicit SQLite tuning.
func TestAKSFollowupLoad(t *testing.T) {
	if os.Getenv("EVALSI_AKS_FOLLOWUP_LOAD") != "1" {
		t.Skip("set EVALSI_AKS_FOLLOWUP_LOAD=1 for the isolated follow-up")
	}
	mode := os.Getenv("EVALSI_AKS_SQLITE_SYNCHRONOUS")
	if mode != "full" && mode != "normal" {
		t.Fatal("EVALSI_AKS_SQLITE_SYNCHRONOUS must be full or normal")
	}
	u := os.Getenv("EVALSI_LOAD_CLICKHOUSE_URL")
	e := start(t, func(c *config.Config) {
		c.OTLP.Grace = config.Default().OTLP.Grace
		c.OTLP.MaxTraces = 100000
		c.Sinks = nil
		c.Evaluate = config.Default().Evaluate
		c.Storage.SQLite = &config.SQLite{Synchronous: mode}
		if u != "" {
			c.Storage.ClickHouse = &store.ClickHouseConfig{
				URL: u, Database: fmt.Sprintf("evalsi_followup_%d", time.Now().UnixNano()),
			}
		}
	})
	report := map[string]any{
		"trace_store": "sqlite", "sqlite_synchronous": mode,
		"threshold_stored_spans_per_s": 20000, "threshold_freshness_p95_s": 120,
	}
	if u != "" {
		report["trace_store"] = "clickhouse"
	}
	defer func() {
		out, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			t.Error(err)
			return
		}
		t.Logf("follow-up report:\n%s", out)
		if path := os.Getenv("EVALSI_LOAD_REPORT"); path != "" {
			if err := os.WriteFile(path, out, 0o600); err != nil {
				t.Error(err)
			}
		}
	}()
	t.Run("EvaluateOverhead", func(t *testing.T) { evaluateOverhead(t, e, report) })
	t.Run("OTLPIngest", func(t *testing.T) { otlpIngest(t, e, report) })
	t.Run("OnlineFreshness", func(t *testing.T) { onlineFreshness(t, e, report) })
}
