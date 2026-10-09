package ingest

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"

	"connectrpc.com/connect"
	collectortracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// TraceExportProcedure is the OTLP/gRPC method path.
const TraceExportProcedure = "/opentelemetry.proto.collector.trace.v1.TraceService/Export"

// maxBody bounds an OTLP/HTTP request body (after decompression).
const maxBody = 32 << 20

// Assign decides which project an OTLP resource's spans go to and which
// labels they carry. It sees the request context (with the caller's
// principal) and the resource attributes. An error, typically a
// connect.Error with PermissionDenied, rejects the whole export.
type Assign func(ctx context.Context, resource Attrs) (project string, labels map[string]string, err error)

// LabelPrefix marks resource attributes that become trace labels.
const LabelPrefix = "evalsi.label."

// ResourceLabels reads evalsi.label.<key> resource attributes.
func ResourceLabels(resource Attrs) map[string]string {
	out := map[string]string{}
	for k, v := range resource {
		if key, ok := strings.CutPrefix(k, LabelPrefix); ok && key != "" {
			out[key] = fmt.Sprint(v)
		}
	}
	return out
}

// DefaultAssign puts every span in the default project with its resource labels.
func DefaultAssign(_ context.Context, resource Attrs) (string, map[string]string, error) {
	return "", ResourceLabels(resource), nil
}

// Receiver accepts OTLP traces over gRPC and HTTP (protobuf or JSON) and
// hands the spans to an assembler.
type Receiver struct {
	assembler *Assembler
	assign    Assign
	opts      []connect.HandlerOption
	// In a cluster, received spans go to the shared span stream instead of
	// the local assembler (see Forward).
	forward Forward
}

// Forward sends one OTLP resource's spans, with the project and labels its
// credential was assigned, to wherever traces are assembled. An error fails
// the export, so the client retries.
type Forward func(ctx context.Context, rs *tracepb.ResourceSpans, project string, labels map[string]string) error

// SetForward sends spans to forward instead of the local assembler.
func (r *Receiver) SetForward(f Forward) { r.forward = f }

// SpansOfResource rebuilds the spans of one resource that a Forward sent,
// for the assembler.
func SpansOfResource(rs *tracepb.ResourceSpans, project string, labels map[string]string) []Span {
	res := ToAttrs(rs.GetResource().GetAttributes())
	var out []Span
	for _, ss := range rs.GetScopeSpans() {
		for _, sp := range ss.GetSpans() {
			out = append(out, Span{Span: sp, Resource: res, Project: project, Labels: labels})
		}
	}
	return out
}

// NewReceiver builds a receiver. assign may be nil (DefaultAssign).
func NewReceiver(a *Assembler, assign Assign, opts ...connect.HandlerOption) *Receiver {
	if assign == nil {
		assign = DefaultAssign
	}
	return &Receiver{assembler: a, assign: assign, opts: opts}
}

// Register mounts OTLP/gRPC (TraceService/Export) and OTLP/HTTP (/v1/traces) on mux.
func (r *Receiver) Register(mux *http.ServeMux) {
	mux.Handle(TraceExportProcedure, connect.NewUnaryHandler(TraceExportProcedure, r.export, r.opts...))
	mux.HandleFunc("POST /v1/traces", r.serveHTTP)
}

// accept assigns every resource's spans before buffering any, so a rejected
// export adds nothing. Spans dropped for a trace over the span limit are
// reported to the exporter as a partial success (when assembled here; a
// cluster assembles them later, on another replica).
func (r *Receiver) accept(ctx context.Context, msg *collectortracepb.ExportTraceServiceRequest) (*collectortracepb.ExportTraceServiceResponse, error) {
	type assigned struct {
		rs      *tracepb.ResourceSpans
		project string
		labels  map[string]string
	}
	var all []assigned
	for _, rs := range msg.GetResourceSpans() {
		project, labels, err := r.assign(ctx, ToAttrs(rs.GetResource().GetAttributes()))
		if err != nil {
			return nil, err
		}
		all = append(all, assigned{rs, project, labels})
	}
	if r.forward != nil {
		for _, a := range all {
			if err := r.forward(ctx, a.rs, a.project, a.labels); err != nil {
				return nil, connect.NewError(connect.CodeUnavailable, err)
			}
		}
		return &collectortracepb.ExportTraceServiceResponse{}, nil
	}
	var spans []Span
	for _, a := range all {
		spans = append(spans, SpansOfResource(a.rs, a.project, a.labels)...)
	}
	resp := &collectortracepb.ExportTraceServiceResponse{}
	if n := r.assembler.Add(spans); n > 0 {
		resp.PartialSuccess = &collectortracepb.ExportTracePartialSuccess{
			RejectedSpans: n,
			ErrorMessage:  fmt.Sprintf("%d spans dropped: their traces exceed the server's span limit per trace", n),
		}
	}
	return resp, nil
}

func (r *Receiver) export(ctx context.Context, req *connect.Request[collectortracepb.ExportTraceServiceRequest]) (*connect.Response[collectortracepb.ExportTraceServiceResponse], error) {
	resp, err := r.accept(ctx, req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
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
	resp, err := r.accept(req.Context(), msg)
	if err != nil {
		status := http.StatusInternalServerError
		var ce *connect.Error
		if errors.As(err, &ce) {
			status = connectHTTPStatus(ce.Code())
		}
		http.Error(w, err.Error(), status)
		return
	}
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

func connectHTTPStatus(c connect.Code) int {
	switch c {
	case connect.CodeUnauthenticated:
		return http.StatusUnauthorized
	case connect.CodePermissionDenied:
		return http.StatusForbidden
	case connect.CodeInvalidArgument:
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
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
