package ingest

import (
	"context"
	"strings"
	"testing"

	collectortracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

func TestRedactionMasksNestedValuesWithoutChangingOriginal(t *testing.T) {
	redactor, err := NewRedactor(RedactionConfig{Keys: []string{"private_*"}, Patterns: []string{"sentinel-value"}, PII: []string{"email", "us-ssn"}})
	if err != nil {
		t.Fatal(err)
	}
	original := Trace{Spans: []Span{{Span: span(1, 0, "sentinel-value", 0, 1, kv("payload", `{"password":"raw-password","nested":[{"private_note":"raw-note","text":"person@example.invalid 123-45-6789"}]}`)), Resource: Attrs{"metadata.authorization": "raw-token"}, Labels: map[string]string{"private_label": "raw-label"}}}}
	redacted := redactor.Trace(original)
	raw, err := protojson.Marshal(redacted.Spans[0].Span)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"sentinel-value", "raw-password", "raw-note", "person@example.invalid", "123-45-6789"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("raw value survived redaction")
		}
	}
	if redacted.Spans[0].Resource["metadata.authorization"] != Redacted || redacted.Spans[0].Labels["private_label"] != Redacted {
		t.Fatal("resource or label was not masked")
	}
	if original.Spans[0].Span.Name != "sentinel-value" || original.Spans[0].Resource["metadata.authorization"] != "raw-token" {
		t.Fatal("original mutated")
	}
}

func TestRedactionRunsBeforeForwarding(t *testing.T) {
	receiver := NewReceiver(nil, nil)
	forwarded := false
	receiver.SetForward(func(ctx context.Context, resource *tracepb.ResourceSpans, project string, labels map[string]string) error {
		forwarded = true
		raw, err := protojson.Marshal(resource)
		if err != nil {
			return err
		}
		if strings.Contains(string(raw), "raw-password") {
			t.Fatal("unredacted value reached the shared span stream")
		}
		return nil
	})
	_, err := receiver.accept(t.Context(), &collectortracepb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{span(1, 0, "agent", 0, 1, kv("password", "raw-password"))}}}}}})
	if err != nil || !forwarded {
		t.Fatalf("forward: %v, %v", forwarded, err)
	}
}

func TestRedactionDoesNotTraverseIngressSpansTwice(t *testing.T) {
	redactor, _ := NewRedactor(RedactionConfig{})
	original := span(1, 0, "agent", 0, 1, kv("password", "raw-password"))
	redactor.Message(original)
	trace := Trace{Spans: []Span{{Span: original, redactedBy: redactor}}}
	first := redactor.Trace(trace)
	second := redactor.Trace(first)
	if first.Spans[0].Span != original || second.Spans[0].Span != original || second.redactedBy != redactor {
		t.Fatal("same-config ingress was cloned and traversed again")
	}
	other, _ := NewRedactor(RedactionConfig{Patterns: []string{"agent"}})
	changed := other.Trace(second)
	if changed.Spans[0].Span == original || changed.Spans[0].Span.Name != Redacted {
		t.Fatal("different redactor incorrectly trusted the local marker")
	}
}

func TestSDKMetadataSurvivesNormalizationAfterRedaction(t *testing.T) {
	redactor, _ := NewRedactor(RedactionConfig{})
	trace := Trace{TraceID: "sdk-trace", Spans: []Span{{Span: span(1, 0, "evalsi.log", 0, 1,
		kv("evalsi.metadata", `{"epoch":2,"loss":0.25,"token":"raw-credential"}`))}}}
	record, _ := ToRecord(redactor.Trace(trace))
	if record.Metadata["epoch"].GetNumberValue() != 2 || record.Metadata["loss"].GetNumberValue() != 0.25 || record.Metadata["token"].GetStringValue() != Redacted {
		t.Fatalf("SDK metadata was lost or not redacted: %v", record.Metadata)
	}
}

func TestRedactionConfiguration(t *testing.T) {
	for _, config := range []RedactionConfig{{Keys: []string{"["}}, {Patterns: []string{"["}}, {PII: []string{"unknown"}}} {
		if _, err := NewRedactor(config); err == nil {
			t.Fatal("invalid redaction config accepted")
		}
	}
	defaults, err := NewRedactor(RedactionConfig{})
	if err != nil || defaults.text(`{"api_key":"secret"}`) != `{"api_key":"[REDACTED]"}` {
		t.Fatal("default credential-key redaction failed")
	}
}

func TestPIIRedactionDistinguishesNumbersFromCardsAndPhones(t *testing.T) {
	redactor, err := NewRedactor(RedactionConfig{PII: []string{"credit-card", "phone"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"4111 1111 1111 1111", "378282246310005", "+14155552671", "+1 (415) 555-2671", "415-555-2671", "+44 20 7946 0958"} {
		if redactor.Text(value) != Redacted {
			t.Errorf("PII was not masked: %q", value)
		}
	}
	for _, value := range []string{"4111 1111 1111 1112", "1234567890123456", "0000000000000000", "123456789.0123", "2026-10-11", "123 456 789 012", "1234567890"} {
		if redactor.Text(value) != value {
			t.Errorf("ordinary numeric value was masked: %q", value)
		}
	}
}

func TestRedactionPreservesUnmatchedJSONAndDisabledMessages(t *testing.T) {
	redactor, err := NewRedactor(RedactionConfig{Patterns: []string{"REDACTED", "123"}})
	if err != nil {
		t.Fatal(err)
	}
	unchanged := `{ "usage": 99, "text": "public" }`
	if redactor.text(unchanged) != unchanged {
		t.Fatal("unmatched JSON formatting changed")
	}
	changed := redactor.text(`{"usage":123,"text":"[REDACTED]"}`)
	if changed != `{"text":"[REDACTED]","usage":"[REDACTED]"}` || redactor.text(changed) != changed {
		t.Fatal("redacted JSON or marker is not stable")
	}
	disabled, err := NewRedactor(RedactionConfig{Disabled: true})
	if err != nil {
		t.Fatal(err)
	}
	original := span(1, 0, "agent", 0, 1, kv("password", "raw-password"))
	original.ProtoReflect().SetUnknown([]byte{0xa0, 0x06, 0x01})
	copy := proto.Clone(original)
	disabled.Message(copy)
	if !proto.Equal(original, copy) {
		t.Fatal("disabled redaction changed a message")
	}
}

func TestRedactionMetadataBytesEventsAndUnknownFields(t *testing.T) {
	redactor, err := NewRedactor(RedactionConfig{Patterns: []string{"private-sentinel"}, PII: []string{"email", "phone", "credit-card", "us-ssn"}})
	if err != nil {
		t.Fatal(err)
	}
	nested, err := structpb.NewValue(map[string]any{"password": "raw-password", "notes": []any{"private-sentinel"}})
	if err != nil {
		t.Fatal(err)
	}
	record := &evalsiv1alpha1.Record{Metadata: map[string]*structpb.Value{"password": structpb.NewStringValue("raw-password"), "nested": nested}}
	redactor.Message(record)
	encoded, err := protojson.Marshal(record)
	if err != nil || strings.Contains(string(encoded), "raw-password") || strings.Contains(string(encoded), "private-sentinel") {
		t.Fatal("record metadata escaped redaction")
	}
	value := &commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: []byte("private-sentinel")}}
	redactor.Message(value)
	if string(value.GetBytesValue()) != Redacted {
		t.Fatal("UTF-8 byte attribute escaped redaction")
	}
	value.Value = &commonpb.AnyValue_BytesValue{BytesValue: []byte{0xff, 0x01}}
	redactor.Message(value)
	if value.GetStringValue() != Redacted {
		t.Fatal("opaque byte attribute was retained")
	}
	message := span(1, 0, "agent", 0, 1)
	message.Events = []*tracepb.Span_Event{{Name: "private-sentinel", Attributes: []*commonpb.KeyValue{kv("password", "raw-password")}}}
	message.Links = []*tracepb.Span_Link{{Attributes: []*commonpb.KeyValue{kv("notes", "person@example.invalid")}}}
	message.Status = &tracepb.Status{Message: "private-sentinel"}
	message.ProtoReflect().SetUnknown([]byte{0xa0, 0x06, 0x01})
	redactor.Message(message)
	encoded, err = protojson.Marshal(message)
	if err != nil || strings.Contains(string(encoded), "raw-password") || strings.Contains(string(encoded), "private-sentinel") || strings.Contains(string(encoded), "person@example.invalid") || len(message.ProtoReflect().GetUnknown()) != 0 {
		t.Fatal("event, link, status or unknown fields escaped redaction")
	}
	for _, value := range []string{"person@example.invalid", "+1 (415) 555-2671", "4111 1111 1111 1111", "123-45-6789"} {
		if strings.Contains(redactor.text(value), value) {
			t.Fatal("PII class did not match its fixture")
		}
	}
}

func TestRedactionDefaultKeysCoverCommonCredentials(t *testing.T) {
	redactor, err := NewRedactor(RedactionConfig{})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"http.request.header.x-api-key", "http.request.header.cookie", "http.response.header.set-cookie", "token", "gen_ai.openai_api_key", "id_token", "auth.secret", "app.db_password", "proxy-authorization", "vault.private_key"} {
		if !redactor.matches(key) {
			t.Errorf("%q is not redacted by default", key)
		}
	}
	for _, key := range []string{"gen_ai.usage.input_tokens", "gen_ai.usage.output_tokens", "llm.max_tokens", "tool.name", "http.method", "session.id"} {
		if redactor.matches(key) {
			t.Errorf("%q is redacted by default", key)
		}
	}
}
