// Package datasetcatalog shares reviewed immutable datasets across a project.
package datasetcatalog

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	pb "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/auth"
	"github.com/abhishek-rnjn/evals.si/internal/authz"
	"github.com/abhishek-rnjn/evals.si/internal/datasets"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

const IdentityKey = "evalsi.dataset-version"
const MaxVersionBytes = 2 << 20
const MaxRecords = 10000

var digestRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

func Project(project string) string {
	if project == "" {
		return authz.DefaultProject
	}
	return project
}

func actor(ctx context.Context) string {
	if p := auth.PrincipalFrom(ctx); p != nil {
		return p.ID()
	}
	return "anonymous"
}

func invalid(message string) error {
	return connect.NewError(connect.CodeInvalidArgument, errors.New(message))
}

func dbError(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return connect.NewError(connect.CodeNotFound, errors.New("dataset version not found"))
	}
	if errors.Is(err, store.ErrExists) {
		return connect.NewError(connect.CodeAlreadyExists, errors.New("dataset name/version already exists; publish a new version"))
	}
	if errors.Is(err, store.ErrDatasetVersionChanged) {
		return connect.NewError(connect.CodeAborted, err)
	}
	if errors.Is(err, store.ErrDatasetVersionIntegrity) {
		return connect.NewError(connect.CodeDataLoss, err)
	}
	return err
}

func Readable(ctx context.Context, d *pb.DatasetVersion) bool {
	c := d.GetContent()
	if !authz.Can(ctx, "datasets.read", c.GetProject(), authz.DatasetVersionResource(c)) {
		return false
	}
	for _, source := range c.GetAccessSources() {
		if !authz.Can(ctx, "traces.read", source.GetProject(), authz.TraceResource(source)) {
			return false
		}
	}
	return true
}

func allowed(ctx context.Context, action string, content *pb.DatasetVersionContent) error {
	if !authz.Can(ctx, action, content.GetProject(), authz.DatasetVersionResource(content)) {
		return connect.NewError(connect.CodePermissionDenied, errors.New("dataset catalog action is not allowed"))
	}
	return nil
}

func validateKey(project, name, version string) error {
	if !datasets.ValidName(project) || !datasets.ValidName(name) || !datasets.ValidName(version) {
		return invalid("project, name and version must be safe lowercase names of 1-128 characters")
	}
	return nil
}

func validateContent(c *pb.DatasetVersionContent) error {
	if err := validateKey(c.GetProject(), c.GetName(), c.GetVersion()); err != nil {
		return err
	}
	if strings.TrimSpace(c.GetOwner()) == "" || len(c.GetOwner()) > 256 || strings.ContainsAny(c.GetOwner(), "\r\n\x00") {
		return invalid("owner must contain 1-256 bytes without line breaks")
	}
	if c.GetSplit() != "held-out" && c.GetSplit() != "tuning" {
		return invalid("split must be held-out or tuning")
	}
	if len(c.GetDescription()) > 4096 || len(c.GetTags()) > 32 || len(c.GetLabels()) > 32 {
		return invalid("description is limited to 4096 bytes, tags and labels to 32")
	}
	seenTags := map[string]bool{}
	for _, tag := range c.GetTags() {
		if !datasets.ValidName(tag) || seenTags[tag] {
			return invalid("tags must be unique safe lowercase names")
		}
		seenTags[tag] = true
	}
	for key, value := range c.GetLabels() {
		if !datasets.ValidName(key) || len(value) > 256 {
			return invalid("labels need safe keys and values of at most 256 bytes")
		}
	}
	if len(c.GetRecords()) < 1 || len(c.GetRecords()) > MaxRecords {
		return invalid("a dataset version needs 1-10000 records")
	}
	if proto.Size(c) > MaxVersionBytes {
		return connect.NewError(connect.CodeResourceExhausted, errors.New("dataset version content exceeds 2 MiB"))
	}
	ids := map[string]bool{}
	for _, record := range c.GetRecords() {
		id := record.GetId()
		if strings.TrimSpace(id) == "" || len(id) > 256 || ids[id] {
			return invalid("records need unique explicit nonempty IDs of at most 256 bytes")
		}
		ids[id] = true
		if _, exists := record.GetMetadata()[IdentityKey]; exists {
			return invalid("record metadata contains reserved evalsi.dataset-version identity")
		}
	}
	return nil
}

func validatePin(pin string) error {
	if !digestRE.MatchString(pin) {
		return invalid("sha256 requires 64 lowercase hex characters")
	}
	return nil
}

func pageSize(size int32) (int, error) {
	if size < 0 || size > 50 {
		return 0, invalid("page_size must be between 1 and 50")
	}
	if size == 0 {
		return 20, nil
	}
	return int(size), nil
}
