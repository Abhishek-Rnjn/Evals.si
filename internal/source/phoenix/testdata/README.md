# Arize Phoenix 20.20.0 fixtures

Responses recorded from `phoenix serve` 20.20.0 after two OpenInference traces were sent to it over
OTLP/HTTP (project `studio`: an agent span with an LLM call and a tool call; the second trace's tool
fails). `traces_p1`/`traces_p2` are `GET /v1/projects/studio/traces?sort=start_time&order=asc&limit=1&include_spans=true`
(one page each, so the cursor is exercised); `spans_by_trace` is `GET /v1/projects/studio/spans?trace_id=..&trace_id=..`;
`annot_post` is `POST /v1/trace_annotations?sync=true`, and `annot_get` the annotation read back after a
second POST with the same `identifier` updated it in place (decision 0016, item 14).

What they show: `include_spans` returns span summaries without attributes, so spans are fetched by
trace ID; IDs are hex; `openinference.span.kind` arrives as `span_kind`, not as an attribute; resource
attributes (`service.name`) are not returned.
