package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"time"

	"connectrpc.com/connect"
	collectortracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/auth"
	"github.com/abhishek-rnjn/evals.si/internal/ingest"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

type cachedResponse struct {
	*connect.Response[struct{}]
	message proto.Message
}

type procedureRequest struct {
	connect.AnyRequest
	procedure string
}

func (request procedureRequest) Spec() connect.Spec {
	return connect.Spec{Procedure: request.procedure}
}

func (response *cachedResponse) Any() any { return response.message }

func digest(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func validIdempotencyKey(key string) bool {
	if len(key) == 0 || len(key) > 128 {
		return false
	}
	for _, character := range key {
		if character < 33 || character > 126 {
			return false
		}
	}
	return true
}

func (g *gate) idempotent(ctx context.Context, req connect.AnyRequest, next connect.UnaryFunc) (connect.AnyResponse, error) {
	key := req.Header().Get("Idempotency-Key")
	// Inbox/catalog operations use immutable publication and revision CAS. Caching their
	// record responses would retain redacted payloads, and replaying an export
	// could bypass a later edit that revoked approval. Always reconcile live.
	if strings.HasPrefix(req.Spec().Procedure, "/evalsi.v1alpha1.FailureService/") || strings.HasPrefix(req.Spec().Procedure, "/evalsi.v1alpha1.DatasetCatalogService/") || strings.HasPrefix(req.Spec().Procedure, "/evalsi.v1alpha1.ReleaseService/") {
		return next(ctx, req)
	}
	if key == "" || strings.HasSuffix(g.rules[req.Spec().Procedure].action, ".read") {
		return next(ctx, req)
	}
	if !validIdempotencyKey(key) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("Idempotency-Key must contain 1 to 128 visible ASCII characters"))
	}
	if export, ok := req.Any().(*collectortracepb.ExportTraceServiceRequest); ok {
		for _, resource := range export.GetResourceSpans() {
			if _, _, err := g.assignTrace(ctx, ingest.ToAttrs(resource.GetResource().GetAttributes())); err != nil {
				return nil, err
			}
		}
	}
	message, ok := req.Any().(proto.Message)
	if !ok {
		return nil, connect.NewError(connect.CodeInternal, errors.New("idempotency requires a protobuf request"))
	}
	raw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(message)
	if err != nil {
		return nil, err
	}
	project := ""
	if scoped, ok := req.Any().(interface{ GetProject() string }); ok {
		project = scoped.GetProject()
	}
	scope := digest([]byte(auth.PrincipalFrom(ctx).ID() + "\x00" + req.Spec().Procedure + "\x00" + project))
	key = digest([]byte(key))
	fingerprint := digest(raw)
	prior, err := g.store.BeginIdempotency(ctx, scope, key, fingerprint, time.Now())
	if err != nil {
		if errors.Is(err, store.ErrIdempotencyConflict) {
			return nil, connect.NewError(connect.CodeAlreadyExists, err)
		}
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	if !prior.Claimed {
		if !prior.Completed {
			return nil, connect.NewError(connect.CodeUnavailable, errors.New("idempotent request is still running or its outcome is uncertain; reconcile the original operation before using a new key"))
		}
		if prior.Code != 0 {
			return nil, connect.NewError(connect.Code(prior.Code), errors.New(prior.Message))
		}
		typeOf, err := protoregistry.GlobalTypes.FindMessageByName(protoreflect.FullName(prior.ResponseType))
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		cached := typeOf.New().Interface()
		if err := proto.Unmarshal(prior.Response, cached); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		response := &cachedResponse{Response: connect.NewResponse(&struct{}{}), message: cached}
		response.Header().Set("Idempotency-Replayed", "true")
		return response, nil
	}
	response, callErr := next(ctx, req)
	if callErr != nil {
		// A failed call is not an outcome to replay: it may be transient or fixable
		// (a quota, a dropped connection), so the key is freed for the retry.
		g.release(ctx, scope, key, fingerprint)
		return response, callErr
	}
	cached := proto.Clone(response.Any().(proto.Message))
	if credentials, ok := cached.(*evalsiv1alpha1.CreateAPIKeyResponse); ok {
		credentials.Secret = ""
	}
	encoded, err := proto.Marshal(cached)
	if err != nil {
		return nil, err
	}
	if len(encoded) > 64<<20 {
		return nil, connect.NewError(connect.CodeResourceExhausted, errors.New("operation finished but its response exceeds the idempotency cache limit; reconcile the original operation"))
	}
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := g.store.CompleteIdempotency(finishCtx, scope, key, fingerprint, string(cached.ProtoReflect().Descriptor().FullName()), encoded, 0, "", time.Now()); err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("operation outcome could not be cached; reconcile the original operation before using a new key"))
	}
	return response, nil
}

// release frees a claim after a failed call. If that fails the claim stays
// until it is reaped, and a retry sees the uncertain-outcome error.
func (g *gate) release(ctx context.Context, scope, key, fingerprint string) {
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = g.store.ReleaseIdempotency(releaseCtx, scope, key, fingerprint)
}

type bufferedIdempotentStream struct {
	connect.StreamingHandlerConn
	requests     [][]byte
	responses    bytes.Buffer
	responseType string
	next         int
}

func (stream *bufferedIdempotentStream) Receive(message any) error {
	if stream.next == len(stream.requests) {
		return io.EOF
	}
	request, ok := message.(proto.Message)
	if !ok {
		return errors.New("idempotent stream requires protobuf messages")
	}
	err := proto.Unmarshal(stream.requests[stream.next], request)
	stream.next++
	return err
}

func (stream *bufferedIdempotentStream) Send(message any) error {
	response, ok := message.(proto.Message)
	if !ok {
		return errors.New("idempotent stream requires protobuf messages")
	}
	raw, err := proto.Marshal(response)
	if err != nil {
		return err
	}
	if stream.responses.Len()+len(raw)+4 > 64<<20 {
		return connect.NewError(connect.CodeResourceExhausted, errors.New("idempotent stream response exceeds 64 MiB; reconcile the original operation"))
	}
	stream.responseType = string(response.ProtoReflect().Descriptor().FullName())
	_ = binary.Write(&stream.responses, binary.BigEndian, uint32(len(raw)))
	_, _ = stream.responses.Write(raw)
	return stream.StreamingHandlerConn.Send(message)
}

func (g *gate) idempotentStream(ctx context.Context, conn connect.StreamingHandlerConn, check func(any) error, next connect.StreamingHandlerFunc) error {
	key := conn.RequestHeader().Get("Idempotency-Key")
	if !validIdempotencyKey(key) {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("Idempotency-Key must contain 1 to 128 visible ASCII characters"))
	}
	procedure := conn.Spec().Procedure
	parts := strings.Split(strings.TrimPrefix(procedure, "/"), "/")
	if len(parts) != 2 {
		return connect.NewError(connect.CodeInternal, errors.New("invalid stream procedure"))
	}
	descriptor, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(parts[0]))
	if err != nil {
		return err
	}
	method := descriptor.(protoreflect.ServiceDescriptor).Methods().ByName(protoreflect.Name(parts[1]))
	typeOf, err := protoregistry.GlobalTypes.FindMessageByName(method.Input().FullName())
	if err != nil {
		return err
	}
	stream := &bufferedIdempotentStream{StreamingHandlerConn: conn}
	hash := sha256.New()
	total := 0
	for {
		message := typeOf.New().Interface()
		if err := conn.Receive(message); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return err
		}
		if stream.requests == nil {
			if err := check(message); err != nil {
				return err
			}
		}
		raw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(message)
		if err != nil {
			return err
		}
		total += len(raw) + 4
		if total > 64<<20 || len(stream.requests) >= 100001 {
			return connect.NewError(connect.CodeResourceExhausted, errors.New("idempotent stream input exceeds 64 MiB or 100001 messages"))
		}
		_ = binary.Write(hash, binary.BigEndian, uint32(len(raw)))
		_, _ = hash.Write(raw)
		stream.requests = append(stream.requests, raw)
	}
	if len(stream.requests) == 0 {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("idempotent stream requires a request message"))
	}
	scope := digest([]byte(auth.PrincipalFrom(ctx).ID() + "\x00" + procedure + "\x00stream"))
	key = digest([]byte(key))
	fingerprint := hex.EncodeToString(hash.Sum(nil))
	prior, err := g.store.BeginIdempotency(ctx, scope, key, fingerprint, time.Now())
	if errors.Is(err, store.ErrIdempotencyConflict) {
		return connect.NewError(connect.CodeAlreadyExists, err)
	} else if err != nil {
		return connect.NewError(connect.CodeUnavailable, err)
	}
	if !prior.Claimed {
		if !prior.Completed {
			return connect.NewError(connect.CodeUnavailable, errors.New("idempotent stream is running or uncertain; reconcile the original operation"))
		}
		conn.ResponseHeader().Set("Idempotency-Replayed", "true")
		if prior.Code != 0 {
			return connect.NewError(connect.Code(prior.Code), errors.New(prior.Message))
		}
		if len(prior.Response) == 0 {
			return nil
		}
		responseType, err := protoregistry.GlobalTypes.FindMessageByName(protoreflect.FullName(prior.ResponseType))
		if err != nil {
			return err
		}
		reader := bytes.NewReader(prior.Response)
		for reader.Len() > 0 {
			var size uint32
			if err := binary.Read(reader, binary.BigEndian, &size); err != nil || uint64(size) > uint64(reader.Len()) {
				return connect.NewError(connect.CodeInternal, errors.New("invalid cached stream response"))
			}
			raw := make([]byte, int(size))
			_, _ = io.ReadFull(reader, raw)
			message := responseType.New().Interface()
			if err := proto.Unmarshal(raw, message); err != nil {
				return err
			}
			if err := conn.Send(message); err != nil {
				return err
			}
		}
		return nil
	}
	if callErr := next(ctx, stream); callErr != nil {
		g.release(ctx, scope, key, fingerprint)
		return callErr
	}
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := g.store.CompleteIdempotency(finishCtx, scope, key, fingerprint, stream.responseType, stream.responses.Bytes(), 0, "", time.Now()); err != nil {
		return connect.NewError(connect.CodeUnavailable, errors.New("stream outcome could not be cached; reconcile the original operation"))
	}
	return nil
}
