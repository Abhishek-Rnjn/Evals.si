package clickhouse

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"strconv"
	"strings"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/abhishek-rnjn/evals.si/internal/ingest"
	"github.com/abhishek-rnjn/evals.si/internal/source"
)

type spanRow struct {
	TraceID, SpanID, ParentID, StartNS, DurationNS     string
	TraceState, SpanName, SpanKind, ServiceName        string
	ScopeName, ScopeVersion, StatusCode, StatusMessage string
	ResourceAttributes, SpanAttributes                 map[string]string
	EventTimes, EventNames                             []string
	EventAttributes                                    []map[string]string
	LinkTraceIDs, LinkSpanIDs, LinkStates              []string
	LinkAttributes                                     []map[string]string
}

func attributes(m map[string]string) []*commonpb.KeyValue {
	out := make([]*commonpb.KeyValue, 0, len(m))
	for k, v := range m {
		out = append(out, &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}})
	}
	return out
}

func toTrace(id string, rows []spanRow) (ingest.Trace, error) {
	trace := ingest.Trace{TraceID: id}
	seen := map[string]string{}
	root := false
	for _, row := range rows {
		if !strings.EqualFold(row.TraceID, id) || !validID(row.SpanID, 8) {
			return ingest.Trace{}, fmt.Errorf("clickhouse: invalid or mismatched span IDs")
		}
		encoded, _ := json.Marshal(row) // sorts map keys, including event/link maps
		if prior, ok := seen[row.SpanID]; ok {
			if prior != string(encoded) {
				return ingest.Trace{}, fmt.Errorf("clickhouse: conflicting rows for span %s", row.SpanID)
			}
			continue
		}
		seen[row.SpanID] = string(encoded)
		start, err := strconv.ParseInt(row.StartNS, 10, 64)
		if err != nil || start < 0 {
			return ingest.Trace{}, fmt.Errorf("clickhouse: invalid span timestamp")
		}
		duration, err := strconv.ParseUint(row.DurationNS, 10, 64)
		if err != nil || duration > uint64(math.MaxInt64-start) {
			return ingest.Trace{}, fmt.Errorf("clickhouse: invalid span duration")
		}
		sp := &tracepb.Span{Name: row.SpanName, TraceState: row.TraceState, StartTimeUnixNano: uint64(start), EndTimeUnixNano: uint64(start) + duration}
		sp.TraceId, _ = hex.DecodeString(id)
		sp.SpanId, _ = hex.DecodeString(row.SpanID)
		if row.ParentID != "" && row.ParentID != "0000000000000000" {
			if !validID(row.ParentID, 8) {
				return ingest.Trace{}, fmt.Errorf("clickhouse: invalid parent span ID")
			}
			sp.ParentSpanId, _ = hex.DecodeString(row.ParentID)
		} else {
			root = true
		}
		switch strings.ToLower(row.SpanKind) {
		case "unspecified", "":
		case "internal":
			sp.Kind = tracepb.Span_SPAN_KIND_INTERNAL
		case "server":
			sp.Kind = tracepb.Span_SPAN_KIND_SERVER
		case "client":
			sp.Kind = tracepb.Span_SPAN_KIND_CLIENT
		case "producer":
			sp.Kind = tracepb.Span_SPAN_KIND_PRODUCER
		case "consumer":
			sp.Kind = tracepb.Span_SPAN_KIND_CONSUMER
		default:
			return ingest.Trace{}, fmt.Errorf("clickhouse: unsupported span kind %q", row.SpanKind)
		}
		sp.Status = &tracepb.Status{Message: row.StatusMessage}
		switch strings.ToLower(row.StatusCode) {
		case "unset", "":
		case "ok":
			sp.Status.Code = tracepb.Status_STATUS_CODE_OK
		case "error":
			sp.Status.Code = tracepb.Status_STATUS_CODE_ERROR
		default:
			return ingest.Trace{}, fmt.Errorf("clickhouse: unsupported status code %q", row.StatusCode)
		}
		attrs := maps.Clone(row.SpanAttributes)
		if attrs == nil {
			attrs = map[string]string{}
		}
		if row.ScopeName != "" {
			attrs["otel.scope.name"] = row.ScopeName
		}
		if row.ScopeVersion != "" {
			attrs["otel.scope.version"] = row.ScopeVersion
		}
		sp.Attributes = attributes(attrs)
		res := ingest.Attrs{source.AttrSourceTraceID: id}
		labels := map[string]string{}
		for k, v := range row.ResourceAttributes {
			res[k] = v
			if label, ok := strings.CutPrefix(k, "evalsi.label."); ok && label != "" {
				labels[label] = v
			}
		}
		// Keep the source identity authoritative even if resource metadata names it.
		res[source.AttrSourceTraceID] = id
		if row.ServiceName != "" {
			res["service.name"] = row.ServiceName
		}
		for k, v := range row.SpanAttributes {
			if label, ok := strings.CutPrefix(k, "evalsi.label."); ok && label != "" {
				labels[label] = v
			}
		}
		if len(row.EventTimes) != len(row.EventNames) || len(row.EventTimes) != len(row.EventAttributes) || len(row.LinkTraceIDs) != len(row.LinkSpanIDs) || len(row.LinkTraceIDs) != len(row.LinkStates) || len(row.LinkTraceIDs) != len(row.LinkAttributes) {
			return ingest.Trace{}, fmt.Errorf("clickhouse: inconsistent nested event/link columns")
		}
		for i, ns := range row.EventTimes {
			t, err := strconv.ParseInt(ns, 10, 64)
			if err != nil || t < 0 {
				return ingest.Trace{}, fmt.Errorf("clickhouse: invalid event timestamp")
			}
			sp.Events = append(sp.Events, &tracepb.Span_Event{TimeUnixNano: uint64(t), Name: row.EventNames[i], Attributes: attributes(row.EventAttributes[i])})
		}
		for i, tid := range row.LinkTraceIDs {
			if !validID(tid, 16) || !validID(row.LinkSpanIDs[i], 8) {
				return ingest.Trace{}, fmt.Errorf("clickhouse: invalid link IDs")
			}
			link := &tracepb.Span_Link{TraceState: row.LinkStates[i], Attributes: attributes(row.LinkAttributes[i])}
			link.TraceId, _ = hex.DecodeString(tid)
			link.SpanId, _ = hex.DecodeString(row.LinkSpanIDs[i])
			sp.Links = append(sp.Links, link)
		}
		trace.Spans = append(trace.Spans, ingest.Span{Span: sp, Resource: res, Labels: labels})
	}
	if !root {
		return ingest.Trace{}, fmt.Errorf("clickhouse: trace root disappeared before fetch")
	}
	return trace, nil
}
