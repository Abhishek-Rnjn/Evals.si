package datasetcatalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/auth"
	"github.com/abhishek-rnjn/evals.si/internal/authz"
	"github.com/abhishek-rnjn/evals.si/internal/failures"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

type Service struct{ store *store.Store }

func New(st *store.Store) *Service { return &Service{store: st} }

func Reference(d *pb.DatasetVersion) *pb.DatasetVersionRef {
	c := d.GetContent()
	return &pb.DatasetVersionRef{Project: c.GetProject(), Name: c.GetName(), Version: c.GetVersion(), Sha256: d.GetSha256()}
}

func (s *Service) get(ctx context.Context, project, name, version string) (*pb.DatasetVersion, error) {
	project = Project(project)
	if err := validateKey(project, name, version); err != nil {
		return nil, err
	}
	d, err := s.store.GetDatasetVersion(ctx, project, name, version)
	if err != nil {
		return nil, dbError(err)
	}
	if !Readable(ctx, d) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("dataset version not found"))
	}
	return d, nil
}

func (s *Service) PublishDatasetVersion(ctx context.Context, req *connect.Request[pb.PublishDatasetVersionRequest]) (*connect.Response[pb.PublishDatasetVersionResponse], error) {
	if req.Msg.GetContent() == nil {
		return nil, invalid("content is required")
	}
	c := proto.Clone(req.Msg.GetContent()).(*pb.DatasetVersionContent)
	c.Project = Project(c.GetProject())
	if len(c.GetAccessSources()) != 0 {
		return nil, invalid("access_sources is server-owned")
	}
	if err := allowed(ctx, "datasets.write", c); err != nil {
		return nil, err
	}
	if err := allowed(ctx, "datasets.read", c); err != nil {
		return nil, err
	}
	if len(req.Msg.GetFailures()) != 0 {
		if len(c.GetRecords()) != 0 {
			return nil, invalid("choose records or failure selections, not both")
		}
		response, err := failures.New(s.store).ExportFailures(ctx, connect.NewRequest(&pb.ExportFailuresRequest{Project: c.GetProject(), Selections: req.Msg.GetFailures()}))
		if err != nil {
			return nil, err
		}
		c.Records = response.Msg.GetRecords()
		var ids []string
		for _, selection := range req.Msg.GetFailures() {
			ids = append(ids, selection.GetId())
		}
		cases, err := s.store.SelectedFailures(ctx, c.GetProject(), ids)
		if err != nil {
			return nil, err
		}
		if len(cases) != len(ids) {
			return nil, dbError(store.ErrNotFound)
		}
		boundaries := map[string]*pb.TraceSummary{}
		for _, f := range cases {
			if !failures.Readable(ctx, f) {
				return nil, dbError(store.ErrNotFound)
			}
			data, err := (proto.MarshalOptions{Deterministic: true}).Marshal(f.GetSource())
			if err != nil {
				return nil, err
			}
			sum := sha256.Sum256(data)
			boundaries[hex.EncodeToString(sum[:])] = f.GetSource()
		}
		keys := make([]string, 0, len(boundaries))
		for key := range boundaries {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			c.AccessSources = append(c.AccessSources, boundaries[key])
		}
	}
	if err := validateContent(c); err != nil {
		return nil, err
	}
	sort.Strings(c.Tags)
	digest, err := store.DatasetContentHash(c)
	if err != nil {
		return nil, err
	}
	d := &pb.DatasetVersion{Content: c, Sha256: digest, Records: int64(len(c.GetRecords())), State: pb.DatasetVersionState_DATASET_VERSION_STATE_DRAFT, Revision: 1, PublishedBy: actor(ctx), PublishedAt: timestamppb.Now()}
	event := &pb.DatasetVersionEvent{Revision: 1, State: d.GetState(), Actor: d.GetPublishedBy(), Sha256: digest, Time: d.GetPublishedAt()}
	if err := s.store.PublishDatasetVersion(ctx, d, event); err != nil {
		return nil, dbError(err)
	}
	return connect.NewResponse(&pb.PublishDatasetVersionResponse{Dataset: d, Reference: Reference(d)}), nil
}

func (s *Service) GetDatasetVersion(ctx context.Context, req *connect.Request[pb.GetDatasetVersionRequest]) (*connect.Response[pb.GetDatasetVersionResponse], error) {
	m := req.Msg
	d, err := s.get(ctx, m.GetProject(), m.GetName(), m.GetVersion())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&pb.GetDatasetVersionResponse{Dataset: d, Reference: Reference(d)}), nil
}

func (s *Service) ListDatasetVersions(ctx context.Context, req *connect.Request[pb.ListDatasetVersionsRequest]) (*connect.Response[pb.ListDatasetVersionsResponse], error) {
	m := req.Msg
	size, err := pageSize(m.GetPageSize())
	if err != nil {
		return nil, err
	}
	if _, ok := pb.DatasetVersionState_name[int32(m.GetState())]; !ok {
		return nil, invalid("unknown dataset version state")
	}
	if m.GetName() != "" && !validName(m.GetName()) {
		return nil, invalid("invalid dataset name")
	}
	afterName, afterVersion := "", ""
	if m.GetPageToken() != "" {
		parts := strings.Split(m.GetPageToken(), "/")
		if len(parts) != 2 || !validName(parts[0]) || !validName(parts[1]) {
			return nil, invalid("invalid page token")
		}
		afterName, afterVersion = parts[0], parts[1]
	}
	// Scan until we have a full authorized page and one authorized lookahead.
	// Only a returned identity may appear in the continuation token.
	visible := []*pb.DatasetVersion{}
	for len(visible) <= size {
		rows, nextName, nextVersion, err := s.store.ListDatasetVersions(ctx, Project(m.GetProject()), m.GetName(), m.GetOwner(), m.GetState(), 50, afterName, afterVersion)
		if err != nil {
			return nil, dbError(err)
		}
		for _, d := range rows {
			if Readable(ctx, d) {
				visible = append(visible, d)
				if len(visible) > size {
					break
				}
			}
		}
		if len(visible) > size || nextName == "" {
			break
		}
		afterName, afterVersion = nextName, nextVersion
	}
	next := ""
	if len(visible) > size {
		visible = visible[:size]
		last := visible[size-1].GetContent()
		next = last.GetName() + "/" + last.GetVersion()
	}
	return connect.NewResponse(&pb.ListDatasetVersionsResponse{Datasets: visible, NextPageToken: next}), nil
}

func validName(name string) bool { return validateKey("default", name, "v1") == nil }

func (s *Service) ReviewDatasetVersion(ctx context.Context, req *connect.Request[pb.ReviewDatasetVersionRequest]) (*connect.Response[pb.ReviewDatasetVersionResponse], error) {
	m := req.Msg
	d, err := s.get(ctx, m.GetProject(), m.GetName(), m.GetVersion())
	if err != nil {
		return nil, err
	}
	if err := allowed(ctx, "datasets.review", d.GetContent()); err != nil {
		return nil, err
	}
	if err := validatePin(m.GetSha256()); err != nil {
		return nil, err
	}
	if d.GetSha256() != m.GetSha256() {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("dataset digest differs from the reviewed pin"))
	}
	if m.GetExpectedRevision() < 1 {
		return nil, invalid("expected_revision must be positive")
	}
	if d.GetRevision() != m.GetExpectedRevision() {
		return nil, dbError(store.ErrDatasetVersionChanged)
	}
	if strings.TrimSpace(m.GetNote()) == "" || len(m.GetNote()) > 4096 {
		return nil, invalid("review note requires 1-4096 bytes of text")
	}
	if d.GetState() == pb.DatasetVersionState_DATASET_VERSION_STATE_RETIRED {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("retired versions cannot change state"))
	}
	switch m.GetDecision() {
	case pb.DatasetVersionState_DATASET_VERSION_STATE_APPROVED:
		if d.GetState() != pb.DatasetVersionState_DATASET_VERSION_STATE_DRAFT {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("only draft versions can be approved"))
		}
		p := auth.PrincipalFrom(ctx)
		if checker := authz.CheckerFrom(ctx); checker != nil && checker.Engine.Enabled() && (p == nil || p.Anonymous()) {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("approval requires an authenticated principal when authorization is enabled"))
		}
		if p != nil && !p.Anonymous() && p.ID() == d.GetPublishedBy() {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("approval requires a different authenticated principal than the publisher"))
		}
	case pb.DatasetVersionState_DATASET_VERSION_STATE_RETIRED:
	default:
		return nil, invalid("review decision must be APPROVED or RETIRED")
	}
	event := &pb.DatasetVersionEvent{Revision: d.GetRevision() + 1, State: m.GetDecision(), Actor: actor(ctx), Sha256: d.GetSha256(), Time: timestamppb.Now()}
	c := d.GetContent()
	if err := s.store.ReviewDatasetVersion(ctx, c.GetProject(), c.GetName(), c.GetVersion(), d.GetRevision(), m.GetDecision(), actor(ctx), m.GetNote(), event); err != nil {
		return nil, dbError(err)
	}
	d.State, d.Revision, d.ReviewedBy, d.ReviewedAt, d.ReviewNote = m.GetDecision(), event.GetRevision(), event.GetActor(), event.GetTime(), m.GetNote()
	return connect.NewResponse(&pb.ReviewDatasetVersionResponse{Dataset: d}), nil
}

func (s *Service) ListDatasetVersionEvents(ctx context.Context, req *connect.Request[pb.ListDatasetVersionEventsRequest]) (*connect.Response[pb.ListDatasetVersionEventsResponse], error) {
	m := req.Msg
	d, err := s.get(ctx, m.GetProject(), m.GetName(), m.GetVersion())
	if err != nil {
		return nil, err
	}
	size, err := pageSize(m.GetPageSize())
	if err != nil {
		return nil, err
	}
	var before int64
	if m.GetPageToken() != "" {
		before, err = strconv.ParseInt(m.GetPageToken(), 10, 64)
		if err != nil || before < 1 {
			return nil, invalid("invalid event page token")
		}
	}
	c := d.GetContent()
	events, next, err := s.store.DatasetVersionEvents(ctx, c.GetProject(), c.GetName(), c.GetVersion(), size, before)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&pb.ListDatasetVersionEventsResponse{Events: events, NextPageToken: next}), nil
}
