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
	patterns []valuePattern
}

// valuePattern masks the matches of a regular expression. A nil mask replaces
// each whole match; otherwise mask decides what of a match to hide.
type valuePattern struct {
	re   *regexp.Regexp
	mask func(match string) string
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
		"credit-card": `\b[0-9](?:[ -]?[0-9]){12,}\b`,
		"phone":       `\+[1-9][0-9]{6,14}\b|\+[1-9][0-9]{0,2}[ -](?:\([0-9]{2,4}\)|[0-9]{2,4})[ -][0-9]{2,4}[ -][0-9]{2,4}\b|\b[2-9][0-9]{2}[-. ][2-9][0-9]{2}[-. ][0-9]{4}\b|\([2-9][0-9]{2}\) ?[2-9][0-9]{2}[-. ][0-9]{4}\b`,
	}
	for _, class := range config.PII {
		pattern, ok := builtins[class]
		if !ok {
			return nil, fmt.Errorf("trace redaction: unknown PII class")
		}
		entry := valuePattern{re: regexp.MustCompile(pattern)}
		if class == "credit-card" {
			entry.mask = maskCardNumbers
		}
		redactor.patterns = append(redactor.patterns, entry)
	}
	for _, pattern := range patterns {
		compiled, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("trace redaction: invalid value regex")
		}
		redactor.patterns = append(redactor.patterns, valuePattern{re: compiled})
	}
	return redactor, nil
}

// maskCardNumbers hides the card numbers in a run of digits (with single
// spaces or dashes between them). A run that is itself a valid card number is
// hidden whole. Otherwise a card can still sit inside it next to other digits,
// such as a CVV or an expiry, so every window with a real issuer prefix and
// that issuer's length that passes the Luhn check is hidden. Other digit runs
// (timestamps, IDs, a card number with a typo) are left readable.
func maskCardNumbers(run string) string {
	var digits []byte
	var at []int
	for index := 0; index < len(run); index++ {
		if run[index] >= '0' && run[index] <= '9' {
			digits = append(digits, run[index])
			at = append(at, index)
		}
	}
	// Longer runs are identifiers and hashes, not a card with extra digits.
	if len(digits) < 13 || len(digits) > 64 {
		return run
	}
	hidden := make([]bool, len(run))
	found := false
	hide := func(from, to int) {
		for index := at[from]; index <= at[to]; index++ {
			hidden[index] = true
		}
		found = true
	}
	if len(digits) <= 19 && digits[0] != '0' && luhnValid(digits) {
		hide(0, len(digits)-1)
	}
	for start := 0; start+13 <= len(digits); start++ {
		for _, length := range issuerLengths(digits[start:]) {
			if start+length <= len(digits) && luhnValid(digits[start:start+length]) {
				hide(start, start+length-1)
			}
		}
	}
	if !found {
		return run
	}
	var out strings.Builder
	for index := 0; index < len(run); index++ {
		if !hidden[index] {
			out.WriteByte(run[index])
		} else if index == 0 || !hidden[index-1] {
			out.WriteString(Redacted)
		}
	}
	return out.String()
}

// issuerLengths returns the card lengths that a number starting with these
// digits can have, or none when no major issuer uses the prefix.
func issuerLengths(digits []byte) []int {
	prefix := func(length int) int {
		value := 0
		for _, digit := range digits[:min(length, len(digits))] {
			value = value*10 + int(digit-'0')
		}
		return value
	}
	switch {
	case digits[0] == '4':
		return []int{16, 19}
	case prefix(2) >= 51 && prefix(2) <= 55, prefix(4) >= 2221 && prefix(4) <= 2720:
		return []int{16}
	case prefix(2) == 34, prefix(2) == 37:
		return []int{15}
	case prefix(2) == 35, prefix(4) == 6011, prefix(2) == 65, prefix(3) >= 644 && prefix(3) <= 649, prefix(3) == 622:
		return []int{16, 17, 18, 19}
	case prefix(2) == 36, prefix(2) == 38, prefix(2) == 39, prefix(3) >= 300 && prefix(3) <= 305:
		return []int{14}
	}
	return nil
}

func luhnValid(digits []byte) bool {
	total, double := 0, false
	for index := len(digits) - 1; index >= 0; index-- {
		digit := int(digits[index] - '0')
		if double {
			digit *= 2
			if digit > 9 {
				digit -= 9
			}
		}
		total += digit
		double = !double
	}
	return total%10 == 0
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
			parts[index] = pattern.re.ReplaceAllStringFunc(part, func(match string) string {
				if pattern.mask != nil {
					return pattern.mask(match)
				}
				return Redacted
			})
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
	if redactor == nil || trace.redactedBy == redactor {
		return trace
	}
	result := trace
	result.redactedBy = redactor
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
		if span.redactedBy == redactor {
			result.Spans[index] = span
			continue
		}
		span.redactedBy = redactor
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
