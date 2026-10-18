-- Execute in the same database as the Collector's otel_traces table.
-- For a different trace table name, replace otel_traces in the table name.
CREATE TABLE IF NOT EXISTS otel_traces_evalsi_scores (
    TraceId String,
    Policy String,
    Metric String,
    Score Nullable(Float64),
    Value Nullable(String),
    Passed Nullable(Bool),
    Rationale String,
    Evaluator String,
    Judged Bool,
    Version UInt64
) ENGINE = ReplacingMergeTree(Version)
ORDER BY (TraceId, Policy, Metric);

-- Read with FINAL for one current row per trace/policy/metric, even before
-- background merges. Replays can leave physical rows until those merges run.
-- SELECT * FROM otel_traces_evalsi_scores FINAL;
