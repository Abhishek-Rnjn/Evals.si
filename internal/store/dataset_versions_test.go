package store

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

func catalogFixture(t *testing.T, project, name, version string) *pb.DatasetVersion {
	t.Helper()
	c := &pb.DatasetVersionContent{Project: project, Name: name, Version: version, Owner: "support-team", Split: "held-out", Records: []*pb.Record{{Id: "case", Input: &pb.Content{Kind: &pb.Content_Text{Text: "input"}}}}}
	digest, err := DatasetContentHash(c)
	if err != nil {
		t.Fatal(err)
	}
	return &pb.DatasetVersion{Content: c, Sha256: digest, Records: 1, State: pb.DatasetVersionState_DATASET_VERSION_STATE_DRAFT, Revision: 1, PublishedBy: "publisher", PublishedAt: timestamppb.Now()}
}

func TestDatasetVersionImmutablePublicationAndConcurrentReview(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	original := catalogFixture(t, "support", "refunds", "v1")
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			results <- s.PublishDatasetVersion(ctx, proto.Clone(original).(*pb.DatasetVersion), &pb.DatasetVersionEvent{Revision: 1})
		})
	}
	wg.Wait()
	close(results)
	wins, duplicates := 0, 0
	for err := range results {
		if err == nil {
			wins++
		} else if errors.Is(err, ErrExists) {
			duplicates++
		} else {
			t.Fatal(err)
		}
	}
	if wins != 1 || duplicates != 7 {
		t.Fatalf("publication wins=%d duplicates=%d", wins, duplicates)
	}
	results = make(chan error, 2)
	for _, state := range []pb.DatasetVersionState{pb.DatasetVersionState_DATASET_VERSION_STATE_APPROVED, pb.DatasetVersionState_DATASET_VERSION_STATE_RETIRED} {
		wg.Go(func() {
			results <- s.ReviewDatasetVersion(ctx, "support", "refunds", "v1", 1, state, "reviewer", "checked", &pb.DatasetVersionEvent{Revision: 2, Time: timestamppb.Now()})
		})
	}
	wg.Wait()
	close(results)
	wins, conflicts := 0, 0
	for err := range results {
		if err == nil {
			wins++
		} else if errors.Is(err, ErrDatasetVersionChanged) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("review wins=%d conflicts=%d", wins, conflicts)
	}
	got, err := s.GetDatasetVersion(ctx, "support", "refunds", "v1")
	if err != nil {
		t.Fatal(err)
	}
	if got.GetRevision() != 2 || !proto.Equal(got.GetContent(), original.GetContent()) || got.GetSha256() != original.GetSha256() {
		t.Fatalf("review changed content: %v", got)
	}
	events, next, err := s.DatasetVersionEvents(ctx, "support", "refunds", "v1", 1, 0)
	if err != nil || len(events) != 1 || next != "2" {
		t.Fatalf("events: %v %q %v", events, next, err)
	}
	events, next, err = s.DatasetVersionEvents(ctx, "support", "refunds", "v1", 1, 2)
	if err != nil || len(events) != 1 || next != "" {
		t.Fatalf("event tail: %v %q %v", events, next, err)
	}
}

func TestDatasetVersionReviewAndHistoryRollbackTogether(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	d := catalogFixture(t, "support", "refunds", "v1")
	if err := s.PublishDatasetVersion(ctx, d, &pb.DatasetVersionEvent{Revision: 1}); err != nil {
		t.Fatal(err)
	}
	// Inject a competing history row so the review event cannot commit.
	if _, err := s.db.ExecContext(ctx, `INSERT INTO dataset_version_events(project,name,version,revision,body) VALUES(?,?,?,?,?)`, "support", "refunds", "v1", 2, marshal(&pb.DatasetVersionEvent{Revision: 2})); err != nil {
		t.Fatal(err)
	}
	if err := s.ReviewDatasetVersion(ctx, "support", "refunds", "v1", 1, pb.DatasetVersionState_DATASET_VERSION_STATE_APPROVED, "reviewer", "checked", &pb.DatasetVersionEvent{Revision: 2}); err == nil {
		t.Fatal("expected history insertion failure")
	}
	got, err := s.GetDatasetVersion(ctx, "support", "refunds", "v1")
	if err != nil || !proto.Equal(got, d) {
		t.Fatalf("review committed without history: %v %v", got, err)
	}
}

func TestDatasetVersionHashCoversOrderedContentAndDetectsTampering(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	d := catalogFixture(t, "support", "refunds", "v1")
	if err := s.PublishDatasetVersion(ctx, d, &pb.DatasetVersionEvent{Revision: 1}); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*pb.DatasetVersionContent){
		func(c *pb.DatasetVersionContent) { c.Owner = "another-team" },
		func(c *pb.DatasetVersionContent) { c.Version = "v2" },
		func(c *pb.DatasetVersionContent) { c.Split = "tuning" },
		func(c *pb.DatasetVersionContent) { c.Tags = []string{"regression"} },
		func(c *pb.DatasetVersionContent) {
			c.AccessSources = []*pb.TraceSummary{{Project: "support", Service: "private-source"}}
		},
		func(c *pb.DatasetVersionContent) {
			c.Records[0].Provenance = &pb.Provenance{Source: &pb.Provenance_Trace{Trace: &pb.TraceProvenance{TraceId: "source"}}}
		},
		func(c *pb.DatasetVersionContent) {
			c.Records[0].Reference = &pb.Content{Kind: &pb.Content_Text{Text: "changed"}}
		},
		func(c *pb.DatasetVersionContent) { c.Labels = map[string]string{"classification": "private"} },
	} {
		content := proto.Clone(d.GetContent()).(*pb.DatasetVersionContent)
		mutate(content)
		hash, err := DatasetContentHash(content)
		if err != nil || hash == d.GetSha256() {
			t.Fatalf("unbound content: %v %v", content, err)
		}
	}
	ordered := proto.Clone(d.GetContent()).(*pb.DatasetVersionContent)
	ordered.Records = append(ordered.Records, &pb.Record{Id: "second"})
	first, _ := DatasetContentHash(ordered)
	ordered.Records[0], ordered.Records[1] = ordered.Records[1], ordered.Records[0]
	second, _ := DatasetContentHash(ordered)
	if first == second {
		t.Fatal("record order was not bound by the digest")
	}
	content := proto.Clone(d.GetContent()).(*pb.DatasetVersionContent)
	content.Records[0].Metadata = map[string]*structpb.Value{"z": structpb.NewStringValue("z"), "a": structpb.NewNumberValue(0)}
	a, _ := DatasetContentHash(content)
	content.Records[0].Metadata = map[string]*structpb.Value{"a": structpb.NewNumberValue(0), "z": structpb.NewStringValue("z")}
	b, _ := DatasetContentHash(content)
	if a != b {
		t.Fatal("map iteration changed the digest")
	}
	d.Content.Owner = "tampered"
	if _, err := s.db.ExecContext(ctx, `UPDATE dataset_versions SET body=? WHERE project=? AND name=? AND version=?`, marshal(d), "support", "refunds", "v1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetDatasetVersion(ctx, "support", "refunds", "v1"); !errors.Is(err, ErrDatasetVersionIntegrity) {
		t.Fatalf("tampered get: %v", err)
	}
	if _, _, _, err := s.ListDatasetVersions(ctx, "support", "", "", 0, 20, "", ""); !errors.Is(err, ErrDatasetVersionIntegrity) {
		t.Fatalf("tampered list: %v", err)
	}
}

func TestDatasetVersionPagesFiltersProjectIsolationAndRestart(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	for _, project := range []string{"support", "checkout"} {
		for _, version := range []string{"v1", "v2", "v3"} {
			d := catalogFixture(t, project, "refunds", version)
			if err := s.PublishDatasetVersion(ctx, d, &pb.DatasetVersionEvent{Revision: 1}); err != nil {
				t.Fatal(err)
			}
		}
	}
	name, version := "", ""
	seen := map[string]bool{}
	for {
		rows, nextName, nextVersion, err := s.ListDatasetVersions(ctx, "support", "refunds", "support-team", pb.DatasetVersionState_DATASET_VERSION_STATE_DRAFT, 1, name, version)
		if err != nil || len(rows) != 1 {
			t.Fatalf("page: %v %v", rows, err)
		}
		c := rows[0].GetContent()
		if c.GetProject() != "support" || len(c.GetRecords()) != 0 || seen[c.GetVersion()] {
			t.Fatalf("bad page: %v", rows)
		}
		seen[c.GetVersion()] = true
		if nextName == "" {
			break
		}
		name, version = nextName, nextVersion
	}
	if len(seen) != 3 {
		t.Fatal(seen)
	}
	if _, err := s.GetDatasetVersion(ctx, "another-project", "refunds", "v1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-project read: %v", err)
	}
	path := filepath.Join(t.TempDir(), "catalog.db")
	persistent, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	d := catalogFixture(t, "support", "refunds", "v1")
	if err := persistent.PublishDatasetVersion(ctx, d, &pb.DatasetVersionEvent{Revision: 1}); err != nil {
		t.Fatal(err)
	}
	if err := persistent.Close(); err != nil {
		t.Fatal(err)
	}
	persistent, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = persistent.Close() })
	got, err := persistent.GetDatasetVersion(ctx, "support", "refunds", "v1")
	if err != nil || !proto.Equal(got, d) {
		t.Fatalf("restart lost dataset: %v %v", got, err)
	}
}
