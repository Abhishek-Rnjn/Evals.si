package datasets

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"sync"

	"connectrpc.com/connect"
	"github.com/parquet-go/parquet-go"
	"github.com/parquet-go/parquet-go/encoding/thrift"
	"github.com/parquet-go/parquet-go/format"
	"google.golang.org/protobuf/encoding/protojson"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/objstore"
)

const MaxUploadBytes int64 = 64 << 20
const MaxUploadRecords = 100000
const MaxUploadRowBytes = 4 << 20

type Service struct {
	Root      string
	Objects   *objstore.Client
	admission sync.Once
	uploads   chan struct{}
}

func ValidateUpload(config *evalsiv1alpha1.DatasetUploadConfig) error {
	if !ValidName(config.GetProject()) || !ValidName(config.GetName()) {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("dataset project and name must be safe lowercase names"))
	}
	if config.GetFormat() != "jsonl" && config.GetFormat() != "parquet" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("dataset format must be jsonl or parquet"))
	}
	return nil
}

func (service *Service) Put(ctx context.Context, config *evalsiv1alpha1.DatasetUploadConfig, input io.Reader) (*evalsiv1alpha1.UploadDatasetResponse, error) {
	if err := ValidateUpload(config); err != nil {
		return nil, err
	}
	if service.Root == "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("dataset uploads need datasets_dir"))
	}
	service.admission.Do(func() { service.uploads = make(chan struct{}, 2) })
	select {
	case service.uploads <- struct{}{}:
		defer func() { <-service.uploads }()
	default:
		return nil, connect.NewError(connect.CodeResourceExhausted, errors.New("dataset upload capacity is busy; retry later"))
	}
	source, err := os.CreateTemp("", "evalsi-upload-")
	if err != nil {
		return nil, err
	}
	defer os.Remove(source.Name())
	defer source.Close()
	size, err := io.Copy(source, io.LimitReader(input, MaxUploadBytes+1))
	if err != nil {
		return nil, err
	}
	if size > MaxUploadBytes {
		return nil, connect.NewError(connect.CodeResourceExhausted, errors.New("dataset upload exceeds 64 MiB"))
	}
	output, err := os.CreateTemp("", "evalsi-dataset-")
	if err != nil {
		return nil, err
	}
	defer os.Remove(output.Name())
	defer output.Close()
	count, err := validateDataset(ctx, source, output, config.GetFormat(), size)
	if err != nil {
		return nil, err
	}
	if _, err := output.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	hash := sha256.New()
	outputSize, err := io.Copy(hash, output)
	if err != nil {
		return nil, err
	}
	checksum := hex.EncodeToString(hash.Sum(nil))
	relative := path.Join("uploaded", config.GetProject(), config.GetName(), checksum+".jsonl")
	if _, err := output.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	if location, remote := objstore.Parse(service.Root); remote {
		if service.Objects == nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("S3 datasets_dir needs storage.s3"))
		}
		if _, err := service.Objects.PutReader(ctx, location.Join(relative), output, outputSize, "", true); err != nil && !errors.Is(err, objstore.ErrConflict) {
			return nil, err
		}
	} else if err := publishDataset(service.Root, relative, output); err != nil {
		return nil, err
	}
	return &evalsiv1alpha1.UploadDatasetResponse{Path: relative, Records: int64(count), Sha256: checksum}, nil
}

func publishDataset(directory, relative string, input io.Reader) error {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.MkdirAll(path.Dir(relative), 0o700); err != nil {
		return err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	temporary := path.Join(path.Dir(relative), ".partial-"+hex.EncodeToString(nonce[:]))
	output, err := root.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer root.Remove(temporary)
	if _, err := io.Copy(output, input); err != nil {
		_ = output.Close()
		return err
	}
	if err := output.Sync(); err != nil {
		_ = output.Close()
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	return root.Rename(temporary, relative)
}

func validateDataset(ctx context.Context, input, output *os.File, format string, size int64) (count int, err error) {
	seen := map[string]bool{}
	var written int64
	write := func(raw []byte, row int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if count >= MaxUploadRecords || len(raw) > MaxUploadRowBytes {
			return connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("row %d exceeds dataset row or record limits", row))
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
			return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("row %d: expected a JSON object", row))
		}
		for _, field := range []string{"input", "output", "reference"} {
			var text string
			if value, present := fields[field]; present && json.Unmarshal(value, &text) == nil {
				fields[field], _ = json.Marshal(map[string]string{"text": text})
			}
		}
		raw, err := json.Marshal(fields)
		if err != nil {
			return err
		}
		record := &evalsiv1alpha1.Record{}
		if err := protojson.Unmarshal(raw, record); err != nil {
			return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("row %d: invalid Record fields", row))
		}
		if record.GetId() == "" {
			record.Id = fmt.Sprintf("record-%d", row)
		}
		if seen[record.GetId()] {
			return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("row %d: duplicate record id", row))
		}
		seen[record.GetId()] = true
		encoded, err := Row(record, nil)
		if err != nil {
			return err
		}
		written += int64(len(encoded) + 1)
		if written > MaxUploadBytes {
			return connect.NewError(connect.CodeResourceExhausted, errors.New("normalized dataset exceeds 64 MiB"))
		}
		if _, err := output.Write(append(encoded, '\n')); err != nil {
			return err
		}
		count++
		return nil
	}
	if format == "jsonl" {
		if _, err := input.Seek(0, io.SeekStart); err != nil {
			return 0, err
		}
		scanner := bufio.NewScanner(input)
		scanner.Buffer(make([]byte, 64<<10), MaxUploadRowBytes+1)
		line := 0
		for scanner.Scan() {
			line++
			if strings.TrimSpace(scanner.Text()) == "" {
				continue
			}
			if err := write(scanner.Bytes(), line); err != nil {
				return 0, err
			}
		}
		if err := scanner.Err(); err != nil {
			return 0, connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("line %d: JSONL line exceeds 4 MiB", line+1))
		}
	} else {
		defer func() {
			if recover() != nil {
				count, err = 0, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid or unsupported Parquet schema"))
			}
		}()
		file, err := parquet.OpenFile(input, size, parquet.SkipBloomFilters(true), parquet.SkipPageIndex(true))
		if err != nil {
			return 0, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid Parquet file"))
		}
		if file.NumRows() > MaxUploadRecords {
			return 0, connect.NewError(connect.CodeResourceExhausted, errors.New("Parquet file exceeds 100000 records"))
		}
		if err := validateParquetPages(input, file, size); err != nil {
			return 0, err
		}
		reader := parquet.NewReader(file)
		defer reader.Close()
		rows := make([]parquet.Row, 1)
		for {
			number, readErr := reader.ReadRows(rows)
			if number > 0 {
				value := make(map[string]any)
				if err := file.Schema().Reconstruct(&value, rows[0]); err != nil {
					return 0, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("row %d: invalid Parquet record", count+1))
				}
				raw, err := json.Marshal(value)
				if err != nil {
					return 0, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("row %d: unsupported Parquet value", count+1))
				}
				if err := write(raw, count+1); err != nil {
					return 0, err
				}
			}
			if errors.Is(readErr, io.EOF) {
				break
			}
			if readErr != nil {
				return 0, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("row %d: corrupt Parquet data", count+1))
			}
		}
	}
	if count == 0 {
		return 0, connect.NewError(connect.CodeInvalidArgument, errors.New("dataset has no records"))
	}
	return count, nil
}

func validateParquetPages(input io.ReaderAt, file *parquet.File, size int64) error {
	reject := func() error {
		return connect.NewError(connect.CodeResourceExhausted, errors.New("Parquet schema or pages exceed upload decoding limits"))
	}
	metadata := file.Metadata()
	if len(metadata.Schema) > 128 || len(metadata.RowGroups) > 1024 {
		return reject()
	}
	for _, field := range metadata.Schema {
		if field.TypeLength.Valid && (field.TypeLength.V < 0 || field.TypeLength.V > 1024) {
			return reject()
		}
	}
	var total int64
	for _, group := range metadata.RowGroups {
		if len(group.Columns) > 64 {
			return reject()
		}
		for _, column := range group.Columns {
			chunk := column.MetaData
			offset := chunk.DataPageOffset
			if chunk.DictionaryPageOffset > 0 && chunk.DictionaryPageOffset < offset {
				offset = chunk.DictionaryPageOffset
			}
			if column.FilePath != "" || column.CryptoMetadata.Value != nil || offset < 4 || chunk.TotalCompressedSize < 0 || offset > size-chunk.TotalCompressedSize {
				return reject()
			}
			reader := bufio.NewReader(io.NewSectionReader(input, offset, chunk.TotalCompressedSize))
			protocol := thrift.CompactProtocol{}
			decoder := thrift.NewDecoder(protocol.NewReader(reader))
			for {
				if _, err := reader.Peek(1); errors.Is(err, io.EOF) {
					break
				} else if err != nil {
					return reject()
				}
				var page format.PageHeader
				if err := decoder.Decode(&page); err != nil {
					return reject()
				}
				if page.CompressedPageSize < 0 || page.CompressedPageSize > MaxUploadRowBytes || page.UncompressedPageSize < 0 || page.UncompressedPageSize > MaxUploadRowBytes {
					return reject()
				}
				for _, values := range []int32{page.DataPageHeader.V.NumValues, page.DataPageHeaderV2.V.NumValues, page.DictionaryPageHeader.V.NumValues} {
					if values < 0 || values > MaxUploadRecords {
						return reject()
					}
				}
				total += int64(page.UncompressedPageSize)
				if total > MaxUploadBytes {
					return reject()
				}
				if _, err := io.CopyN(io.Discard, reader, int64(page.CompressedPageSize)); err != nil {
					return reject()
				}
			}
		}
	}
	return nil
}

func (service *Service) UploadDataset(ctx context.Context, stream *connect.ClientStream[evalsiv1alpha1.UploadDatasetRequest]) (*connect.Response[evalsiv1alpha1.UploadDatasetResponse], error) {
	if !stream.Receive() || stream.Msg().GetConfig() == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("upload must start with a config message"))
	}
	config := stream.Msg().GetConfig()
	input := &uploadReader{stream: stream}
	response, err := service.Put(ctx, config, input)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(response), nil
}

type uploadReader struct {
	stream *connect.ClientStream[evalsiv1alpha1.UploadDatasetRequest]
	data   []byte
}

func (reader *uploadReader) Read(output []byte) (int, error) {
	for len(reader.data) == 0 {
		if !reader.stream.Receive() {
			if err := reader.stream.Err(); err != nil {
				return 0, err
			}
			return 0, io.EOF
		}
		if reader.stream.Msg().GetConfig() != nil {
			return 0, connect.NewError(connect.CodeInvalidArgument, errors.New("upload config may appear only once"))
		}
		reader.data = reader.stream.Msg().GetData()
	}
	number := copy(output, reader.data)
	reader.data = reader.data[number:]
	return number, nil
}

func (service *Service) ImportDataset(ctx context.Context, request *connect.Request[evalsiv1alpha1.ImportDatasetRequest]) (*connect.Response[evalsiv1alpha1.ImportDatasetResponse], error) {
	config := request.Msg.GetConfig()
	if err := ValidateUpload(config); err != nil {
		return nil, err
	}
	root, configured := objstore.Parse(service.Root)
	location, valid := objstore.Parse(request.Msg.GetUri())
	prefix := root.Join(path.Join("incoming", config.GetProject())).Key + "/"
	if !configured || !valid || service.Objects == nil || location.Bucket != root.Bucket || path.Clean(location.Key) != location.Key || !strings.HasPrefix(location.Key, prefix) {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("imports must name an object under datasets_dir/incoming/<project>/"))
	}
	input, err := service.Objects.Open(ctx, location, MaxUploadBytes)
	if err != nil {
		return nil, err
	}
	defer input.Close()
	result, err := service.Put(ctx, config, input)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&evalsiv1alpha1.ImportDatasetResponse{Path: result.GetPath(), Records: result.GetRecords(), Sha256: result.GetSha256()}), nil
}
