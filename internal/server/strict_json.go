package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"connectrpc.com/connect"
	"connectrpc.com/vanguard"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Both Connect and Vanguard discard unknown JSON fields and enum values by
// default, so a typo ("permisions", an event name) silently becomes the
// field's zero value. Requests are unmarshalled strictly instead; responses
// are written as before.

// connectJSONCodec replaces Connect's "json" codec on the handlers.
type connectJSONCodec struct{}

func (connectJSONCodec) Name() string { return "json" }

func (connectJSONCodec) Marshal(message any) ([]byte, error) {
	m, ok := message.(proto.Message)
	if !ok {
		return nil, fmt.Errorf("%T is not a protobuf message", message)
	}
	return protojson.MarshalOptions{}.Marshal(m)
}

func (connectJSONCodec) Unmarshal(data []byte, message any) error {
	m, ok := message.(proto.Message)
	if !ok {
		return fmt.Errorf("%T is not a protobuf message", message)
	}
	if len(data) == 0 {
		return fmt.Errorf("zero-length payload is not a valid JSON object")
	}
	if err := (protojson.UnmarshalOptions{}).Unmarshal(data, m); err != nil {
		return fmt.Errorf("unmarshal into %T: %w", message, err)
	}
	return nil
}

// restJSONCodec is Vanguard's JSON codec without DiscardUnknown. Its decoding
// errors are the caller's: InvalidArgument rather than Unknown.
type restJSONCodec struct{ vanguard.JSONCodec }

func newRESTJSONCodec(res vanguard.TypeResolver) vanguard.Codec {
	c := vanguard.NewJSONCodec(res)
	c.UnmarshalOptions.DiscardUnknown = false
	return restJSONCodec{*c}
}

func (c restJSONCodec) Unmarshal(data []byte, msg proto.Message) error {
	return invalidArgument(c.JSONCodec.Unmarshal(data, msg))
}

func (c restJSONCodec) UnmarshalField(data []byte, msg proto.Message, field protoreflect.FieldDescriptor) error {
	return invalidArgument(c.JSONCodec.UnmarshalField(data, msg, field))
}

func invalidArgument(err error) error {
	if err == nil {
		return nil
	}
	return connect.NewError(connect.CodeInvalidArgument, err)
}

// unknownQueryParam turns Vanguard's refusal of a query parameter that names
// no request field (reported as Unknown, HTTP 500) into InvalidArgument, 400.
// Vanguard offers no hook for that error, so a 500 is held until it is known
// to be that one; any other 500 is passed on unchanged.
func unknownQueryParam(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hw := &heldErrorWriter{ResponseWriter: w}
		next.ServeHTTP(hw, r)
		hw.finish()
	})
}

type heldErrorWriter struct {
	http.ResponseWriter
	held bool
	body bytes.Buffer
}

func (w *heldErrorWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *heldErrorWriter) WriteHeader(code int) {
	if code == http.StatusInternalServerError {
		w.held = true
		return
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *heldErrorWriter) Write(b []byte) (int, error) {
	if w.held {
		return w.body.Write(b)
	}
	return w.ResponseWriter.Write(b)
}

func (w *heldErrorWriter) Flush() {
	if !w.held {
		_ = http.NewResponseController(w.ResponseWriter).Flush()
	}
}

func (w *heldErrorWriter) finish() {
	if !w.held {
		return
	}
	out, status := w.body.Bytes(), http.StatusInternalServerError
	var e map[string]any
	if json.Unmarshal(out, &e) == nil && strings.HasPrefix(fmt.Sprint(e["message"]), "unknown field in field path") {
		e["code"] = float64(connect.CodeInvalidArgument)
		out, _ = json.Marshal(e)
		status = http.StatusBadRequest
		w.Header().Del("Content-Length")
	}
	w.ResponseWriter.WriteHeader(status)
	_, _ = w.ResponseWriter.Write(out)
}
