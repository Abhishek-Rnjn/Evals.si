package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"

	"connectrpc.com/connect"
)

func TestSpoolDatasetUploadBlamesTheClientForReadErrors(t *testing.T) {
	for _, readErr := range []error{io.ErrUnexpectedEOF, iotest.ErrTimeout} {
		_, _, err := spoolDatasetUpload(io.MultiReader(strings.NewReader("a,b"), iotest.ErrReader(readErr)))
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("%v: code %v, want invalid argument (a client fault)", readErr, connect.CodeOf(err))
		}
		recorder := httptest.NewRecorder()
		datasetHTTPError(recorder, err)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%v: HTTP %d, want 400", readErr, recorder.Code)
		}
	}
	file, checksum, err := spoolDatasetUpload(strings.NewReader("a,b"))
	if err != nil || checksum == "" {
		t.Fatalf("a good body failed: %v", err)
	}
	_ = file.Close()
}
