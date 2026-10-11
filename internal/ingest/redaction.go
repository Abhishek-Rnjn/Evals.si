package ingest

import (
	"encoding/json"
	"fmt"
	"path"
	"reflect"
	"regexp"
	"strings"
	"unicode/utf8"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/structpb"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

type RedactionConfig struct {
	Disabled bool     `json:"disabled"`
	Keys     []string `json:"keys"`
	Patterns []string `json:"patterns"`
	PII      []string `json:"pii"`
}

type Redactor struct {
	keys     []string
	patterns []*regexp.Regexp
}

const Redacted = "[REDACTED]"

// defaultKeys are matched, as globs, against a whole attribute or field name
// and against its last dot-separated segment, so http.request.header.x-api-key
// matches x-api-key. Token counts (input_tokens) are plural and not matched.
var defaultKeys = []string{
	"password", "passwd", "*_password", "*-password",
	"authorization", "proxy-authorization", "cookie", "set-cookie",
	"token", "*_token", "*-token", "secret", "*_secret", "*-secret",
	"api_key", "api-key", "apikey", "*_api_key", "*-api-key", "*_apikey",
	"private_key", "*_private_key", "*-private-key",
}

func NewRedactor(config RedactionConfig) (*Redactor, error) {
	if config.Disabled {
		return &Redactor{}, nil
	}
	keys := append([]string(nil), defaultKeys...)
	keys = append(keys, config.Keys...)
	redactor := &Redactor{}
	for _, key := range keys {
		key = strings.ToLower(key)
		if _, err := path.Match(key, ""); err != nil {
			return nil, fmt.Errorf("trace redaction: invalid key pattern")
		}
		redactor.keys = append(redactor.keys, key)
	}
	patterns := append([]string(nil), config.Patterns...)
	builtins := map[string]string{
		"email":       `(?i)\b[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}\b`,
		"us-ssn":      `\b[0-9]{3}-[0-9]{2}-[0-9]{4}\b`,
		"credit-card": `\b(?:[0-9][ -]?){13,19}\b`,
		"phone":       `\+?[0-9][0-9 ().-]{7,}[0-9]`,
	}
	for _, class := range config.PII {
		pattern, ok := builtins[class]
		if !ok {
			return nil, fmt.Errorf("trace redaction: unknown PII class")
		}
		patterns = append(patterns, pattern)
	}
	for _, pattern := range patterns {
		compiled, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("trace redaction: invalid value regex")
		}
		redactor.patterns = append(redactor.patterns, compiled)
	}
	return redactor, nil
}

func (redactor *Redactor) matches(key string) bool {
	key = strings.ToLower(key)
	for _, pattern := range redactor.keys {
		if matched, _ := path.Match(pattern, key); matched {
			return true
		}
		if matched, _ := path.Match(pattern, key[strings.LastIndex(key, ".")+1:]); matched {
			return true
		}
	}
	return false
}

func (redactor *Redactor) text(value string) string {
	if len(redactor.keys) == 0 && len(redactor.patterns) == 0 {
		return value
	}
	var structured any
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.UseNumber()
	if json.Valid([]byte(value)) && decoder.Decode(&structured) == nil {
		switch structured.(type) {
		case map[string]any, []any:
			changed := redactor.value(structured)
			if !reflect.DeepEqual(structured, changed) {
				raw, err := json.Marshal(changed)
				if err == nil {
					value = string(raw)
				}
			}
			return value
		}
	}
	for _, pattern := range redactor.patterns {
		parts := strings.Split(value, Redacted)
		for index, part := range parts {
			parts[index] = pattern.ReplaceAllLiteralString(part, Redacted)
		}
		value = strings.Join(parts, Redacted)
	}
	return value
}

func (redactor *Redactor) Text(value string) string {
	if redactor == nil {
		return value
	}
	return redactor.text(value)
}

func (redactor *Redactor) value(value any) any {
	switch typed := value.(type) {
	case string:
		return redactor.text(typed)
	case json.Number:
		if changed := redactor.text(typed.String()); changed != typed.String() {
			return changed
		}
		return typed
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, child := range typed {
			if redactor.matches(key) {
				result[key] = Redacted
			} else {
				result[key] = redactor.value(child)
			}
		}
		return result
	case []any:
		result := make([]any, len(typed))
		for index, child := range typed {
			result[index] = redactor.value(child)
		}
		return result
	default:
		return value
	}
}

func (redactor *Redactor) Message(message proto.Message) {
	if redactor == nil || message == nil {
		return
	}
	redactor.message(message.ProtoReflect())
}

func (redactor *Redactor) message(message protoreflect.Message) {
	if len(redactor.keys) == 0 && len(redactor.patterns) == 0 {
		return
	}
	message.SetUnknown(nil)
	if value, ok := message.Interface().(*commonpb.AnyValue); ok {
		if raw, ok := value.Value.(*commonpb.AnyValue_BytesValue); ok {
			if utf8.Valid(raw.BytesValue) {
				raw.BytesValue = []byte(redactor.text(string(raw.BytesValue)))
			} else {
				value.Value = &commonpb.AnyValue_StringValue{StringValue: Redacted}
			}
			return
		}
	}
	if pair, ok := message.Interface().(*commonpb.KeyValue); ok {
		if redactor.matches(pair.Key) {
			pair.Value = &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: Redacted}}
		} else if pair.Value != nil {
			redactor.Message(pair.Value)
		}
		return
	}
	message.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		if field.IsMap() {
			values := value.Map()
			values.Range(func(key protoreflect.MapKey, child protoreflect.Value) bool {
				if redactor.matches(key.String()) {
					if field.MapValue().Kind() == protoreflect.StringKind {
						values.Set(key, protoreflect.ValueOfString(Redacted))
					} else if field.MapValue().Message().FullName() == "google.protobuf.Value" {
						values.Set(key, protoreflect.ValueOfMessage(structpb.NewStringValue(Redacted).ProtoReflect()))
					}
				} else if field.MapValue().Kind() == protoreflect.MessageKind {
					redactor.message(child.Message())
				} else if field.MapValue().Kind() == protoreflect.StringKind {
					values.Set(key, protoreflect.ValueOfString(redactor.text(child.String())))
				}
				return true
			})
		} else if field.IsList() {
			list := value.List()
			for index := 0; index < list.Len(); index++ {
				if field.Kind() == protoreflect.MessageKind {
					redactor.message(list.Get(index).Message())
				} else if field.Kind() == protoreflect.StringKind {
					list.Set(index, protoreflect.ValueOfString(redactor.text(list.Get(index).String())))
				}
			}
		} else if field.Kind() == protoreflect.MessageKind {
			redactor.message(value.Message())
		} else if field.Kind() == protoreflect.StringKind {
			message.Set(field, protoreflect.ValueOfString(redactor.text(value.String())))
		}
		return true
	})
}

func (redactor *Redactor) Trace(trace Trace) Trace {
	if redactor == nil {
		return trace
	}
	result := trace
	if trace.Adjust != nil {
		result.Adjust = func(record *evalsiv1alpha1.Record, info *TraceInfo) {
			trace.Adjust(record, info)
			redactor.Message(record)
			info.Service = redactor.text(info.Service)
			info.Name = redactor.text(info.Name)
			info.Attributes = Attrs(redactor.value(map[string]any(info.Attributes)).(map[string]any))
			info.Resource = Attrs(redactor.value(map[string]any(info.Resource)).(map[string]any))
			for index, value := range info.Tools {
				info.Tools[index] = redactor.text(value)
			}
			for index, value := range info.Models {
				info.Models[index] = redactor.text(value)
			}
			for key, value := range info.Labels {
				if redactor.matches(key) {
					info.Labels[key] = Redacted
				} else {
					info.Labels[key] = redactor.text(value)
				}
			}
		}
	}
	result.Spans = make([]Span, len(trace.Spans))
	for index, span := range trace.Spans {
		span.Span = proto.Clone(span.Span).(*tracepb.Span)
		redactor.Message(span.Span)
		resource := make(Attrs, len(span.Resource))
		for key, value := range span.Resource {
			if redactor.matches(key) {
				resource[key] = Redacted
			} else {
				resource[key] = redactor.value(value)
			}
		}
		span.Resource = resource
		labels := make(map[string]string, len(span.Labels))
		for key, value := range span.Labels {
			if redactor.matches(key) {
				labels[key] = Redacted
			} else {
				labels[key] = redactor.text(value)
			}
		}
		span.Labels = labels
		result.Spans[index] = span
	}
	return result
}
