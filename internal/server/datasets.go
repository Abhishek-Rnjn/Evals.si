package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1/evalsiv1alpha1connect"
	"github.com/abhishek-rnjn/evals.si/internal/datasets"
)

type datasetUploadRequest struct {
	*connect.Request[evalsiv1alpha1.ImportDatasetRequest]
}

func (request datasetUploadRequest) Spec() connect.Spec {
	return connect.Spec{Procedure: evalsiv1alpha1connect.DatasetServiceUploadDatasetProcedure}
}

func registerDatasetUpload(mux *http.ServeMux, dependencies deps) {
	mux.HandleFunc("POST /v1alpha1/datasets/{name}/upload", func(writer http.ResponseWriter, request *http.Request) {
		config := &evalsiv1alpha1.DatasetUploadConfig{Project: request.URL.Query().Get("project"), Name: request.PathValue("name"), Format: request.URL.Query().Get("format")}
		ctx, checker, err := dependencies.gate.authenticate(request.Context(), evalsiv1alpha1connect.DatasetServiceUploadDatasetProcedure, "http", request.RemoteAddr, request.Header)
		if err == nil {
			err = dependencies.gate.check(ctx, checker, evalsiv1alpha1connect.DatasetServiceUploadDatasetProcedure, &evalsiv1alpha1.UploadDatasetRequest{Part: &evalsiv1alpha1.UploadDatasetRequest_Config{Config: config}})
		}
		if err != nil {
			datasetHTTPError(writer, err)
			return
		}
		if dependencies.datasets == nil {
			datasetHTTPError(writer, connect.NewError(connect.CodeFailedPrecondition, errors.New("dataset uploads are not configured")))
			return
		}
		request.Body = http.MaxBytesReader(writer, request.Body, datasets.MaxUploadBytes+1)
		var result *evalsiv1alpha1.UploadDatasetResponse
		if key := request.Header.Get("Idempotency-Key"); key != "" {
			if !validIdempotencyKey(key) {
				datasetHTTPError(writer, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid Idempotency-Key")))
				return
			}
			raw, readErr := io.ReadAll(io.LimitReader(request.Body, datasets.MaxUploadBytes+1))
			if readErr != nil || int64(len(raw)) > datasets.MaxUploadBytes {
				datasetHTTPError(writer, connect.NewError(connect.CodeResourceExhausted, errors.New("dataset upload exceeds 64 MiB")))
				return
			}
			keyed := datasetUploadRequest{Request: connect.NewRequest(&evalsiv1alpha1.ImportDatasetRequest{Config: config, Uri: "sha256:" + digest(raw)})}
			keyed.Header().Set("Idempotency-Key", key)
			response, callErr := dependencies.gate.idempotent(ctx, keyed, func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
				uploaded, err := dependencies.datasets.Put(ctx, config, bytes.NewReader(raw))
				if err != nil {
					return nil, err
				}
				return connect.NewResponse(uploaded), nil
			})
			err = callErr
			if err == nil {
				result = response.Any().(*evalsiv1alpha1.UploadDatasetResponse)
				if response.Header().Get("Idempotency-Replayed") != "" {
					writer.Header().Set("Idempotency-Replayed", "true")
				}
			}
		} else {
			result, err = dependencies.datasets.Put(ctx, config, request.Body)
		}
		if err != nil {
			datasetHTTPError(writer, err)
			return
		}
		raw, err := protojson.Marshal(result)
		if err != nil {
			datasetHTTPError(writer, err)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusCreated)
		_, _ = writer.Write(raw)
	})
}

func datasetHTTPError(writer http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	switch connect.CodeOf(err) {
	case connect.CodeInvalidArgument:
		code = http.StatusBadRequest
	case connect.CodeUnauthenticated:
		code = http.StatusUnauthorized
	case connect.CodePermissionDenied:
		code = http.StatusForbidden
	case connect.CodeResourceExhausted:
		code = http.StatusRequestEntityTooLarge
	case connect.CodeFailedPrecondition:
		code = http.StatusConflict
	case connect.CodeAlreadyExists:
		code = http.StatusConflict
	case connect.CodeUnavailable:
		code = http.StatusServiceUnavailable
	}
	message := http.StatusText(code)
	if code == http.StatusBadRequest || code == http.StatusRequestEntityTooLarge {
		var failure *connect.Error
		if errors.As(err, &failure) {
			message = failure.Message()
		}
	}
	http.Error(writer, message, code)
}
