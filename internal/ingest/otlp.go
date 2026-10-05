package ingest

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"

	"connectrpc.com/connect"
	collectortracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// TraceExportProcedure is the OTLP/gRPC method path.
const TraceExportProcedure = "/opentelemetry.proto.collector.trace.v1.TraceService/Export"

// maxBody bounds an OTLP/HTTP request body (after decompression).
const maxBody = 32 << 20

// Receiver accepts OTLP traces over gRPC and HTTP (protobuf or JSON) and
// hands the spans to an assembler.
type Receiver struct {
	assembler *Assembler
}

// NewReceiver builds a receiver.
func NewReceiver(a *Assembler) *Receiver { return &Receiver{assembler: a} }

// Register mounts OTLP/gRPC (TraceService/Export) and OTLP/HTTP (/v1/traces) on mux.
func (r *Receiver) Register(mux *http.ServeMux) {
	mux.Handle(TraceExportProcedure, connect.NewUnaryHandler(TraceExportProcedure, r.export))
	mux.HandleFunc("POST /v1/traces", r.serveHTTP)
}

func (r *Receiver) export(_ context.Context, req *connect.Request[collectortracepb.ExportTraceServiceRequest]) (*connect.Response[collectortracepb.ExportTraceServiceResponse], error) {
	r.assembler.Add(SpansOf(req.Msg.GetResourceSpans()))
	return connect.NewResponse(&collectortracepb.ExportTraceServiceResponse{}), nil
}

func (r *Receiver) serveHTTP(w http.ResponseWriter, req *http.Request) {
	var body io.Reader = http.MaxBytesReader(w, req.Body, maxBody)
	if req.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(body)
		if err != nil {
			http.Error(w, "bad gzip body", http.StatusBadRequest)
			return
		}
		defer gz.Close()
		body = io.LimitReader(gz, maxBody)
	}
	raw, err := io.ReadAll(body)
	if err != nil {
		http.Error(w, "reading body: "+err.Error(), http.StatusBadRequest)
		return
	}
	msg := &collectortracepb.ExportTraceServiceRequest{}
	mediaType, _, _ := mime.ParseMediaType(req.Header.Get("Content-Type"))
	switch mediaType {
	case "application/x-protobuf", "application/protobuf":
		err = proto.Unmarshal(raw, msg)
	case "application/json":
		err = UnmarshalOTLPJSON(raw, msg)
	default:
		http.Error(w, "content type must be application/x-protobuf or application/json", http.StatusUnsupportedMediaType)
		return
	}
	if err != nil {
		http.Error(w, "decoding OTLP: "+err.Error(), http.StatusBadRequest)
		return
	}
	r.assembler.Add(SpansOf(msg.GetResourceSpans()))
	resp := &collectortracepb.ExportTraceServiceResponse{}
	if mediaType == "application/json" {
		w.Header().Set("Content-Type", "application/json")
		out, _ := protojson.Marshal(resp)
		_, _ = w.Write(out)
		return
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
	out, _ := proto.Marshal(resp)
	_, _ = w.Write(out)
}

// UnmarshalOTLPJSON decodes OTLP/JSON, which differs from protojson in one
// way that matters: trace and span ids are hex strings, not base64.
func UnmarshalOTLPJSON(raw []byte, msg proto.Message) error {
	var doc any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return err
	}
	if err := hexIDsToBase64(doc); err != nil {
		return err
	}
	fixed, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	return protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(fixed, msg)
}

var idFields = map[string]bool{"traceId": true, "spanId": true, "parentSpanId": true, "trace_id": true, "span_id": true, "parent_span_id": true}

func hexIDsToBase64(v any) error {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if s, ok := val.(string); ok && idFields[k] && s != "" {
				b, err := hex.DecodeString(s)
				if err != nil {
					return fmt.Errorf("%s %q is not hex", k, s)
				}
				x[k] = base64.StdEncoding.EncodeToString(b)
				continue
			}
			if err := hexIDsToBase64(val); err != nil {
				return err
			}
		}
	case []any:
		for _, e := range x {
			if err := hexIDsToBase64(e); err != nil {
				return err
			}
		}
	}
	return nil
}
