package runs

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	pb "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/auth"
	"github.com/abhishek-rnjn/evals.si/internal/authz"
	"github.com/abhishek-rnjn/evals.si/internal/datasetcatalog"
	"github.com/abhishek-rnjn/evals.si/internal/datasets"
	"github.com/abhishek-rnjn/evals.si/internal/store"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ReleaseReadable rechecks the original run/dataset/source boundaries after
// retention. A decision grants no additional access to its source resources.
func ReleaseReadable(ctx context.Context, d *pb.StoredReleaseDecision) bool {
	project := d.GetDecision().GetProject()
	if !authz.Can(ctx, "releases.read", project, authz.RunResourceFor(ctx, d.GetCandidateAccess())) {
		return false
	}
	for _, run := range []*pb.Run{d.GetBaselineAccess(), d.GetCandidateAccess()} {
		if !authz.Can(ctx, "runs.read", run.GetProject(), authz.RunResourceFor(ctx, run)) {
			return false
		}
	}
	return datasetcatalog.Readable(ctx, &pb.DatasetVersion{Content: d.GetDatasetAccess()})
}
func releaseStoreError(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return connect.NewError(connect.CodeNotFound, errors.New("release decision not found"))
	}
	if errors.Is(err, store.ErrExists) {
		return connect.NewError(connect.CodeAlreadyExists, errors.New("release decision ID is immutable; choose a new ID"))
	}
	if errors.Is(err, store.ErrReleaseIntegrity) {
		return connect.NewError(connect.CodeDataLoss, err)
	}
	return err
}
func (m *Manager) CreateReleaseDecision(ctx context.Context, req *connect.Request[pb.CreateReleaseDecisionRequest]) (*connect.Response[pb.CreateReleaseDecisionResponse], error) {
	r := req.Msg
	project := projectOr(r.GetProject())
	policy := r.GetPolicy()
	if !datasets.ValidName(project) || !datasets.ValidName(r.GetId()) {
		return nil, invalid("release project and ID must be safe names")
	}
	if err := validateReleasePolicy(policy); err != nil {
		return nil, err
	}
	if r.GetPolicySha256() != releaseHash("evalsi.release-policy/v1", policy) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("release policy SHA-256 mismatch"))
	}
	if r.GetBaseline().GetRunId() == r.GetCandidate().GetRunId() {
		return nil, invalid("baseline and candidate must be different runs")
	}

	d := &pb.ReleaseDecision{Id: r.GetId(), Project: project, Policy: proto.Clone(policy).(*pb.ReleasePolicy), PolicySha256: r.GetPolicySha256(), Outcome: pb.ReleaseOutcome_RELEASE_OUTCOME_PASS, CreatedBy: auth.PrincipalFrom(ctx).ID(), CreatedAt: timestamppb.Now()}
	snapshots := []*releaseSnapshot{}
	for _, ref := range []*pb.ReleaseRunEvidenceRef{r.GetBaseline(), r.GetCandidate()} {
		if !releasePin.MatchString(ref.GetSha256()) || !releaseCommit.MatchString(ref.GetCommit()) {
			return nil, invalid("run evidence requires a SHA-256 and a declared hexadecimal commit")
		}
		snapshot, err := m.releaseSnapshot(ctx, ref.GetRunId())
		if err != nil {
			return nil, err
		}
		if snapshot.run.GetProject() != project {
			return nil, invalid("both release runs must belong to the release project")
		}
		if !proto.Equal(snapshot.evidence.GetReference(), ref) {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("run evidence or declared commit changed; inspect and repin"))
		}
		snapshots = append(snapshots, snapshot)
	}
	a, b := snapshots[0], snapshots[1]
	if !authz.Can(ctx, "releases.create", project, authz.RunResourceFor(ctx, b.run)) {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("release decisions are not allowed"))
	}
	d.Baseline, d.Candidate = a.evidence, b.evidence
	ref := policy.GetDataset()
	response, err := datasetcatalog.New(m.store).GetDatasetVersion(ctx, connect.NewRequest(&pb.GetDatasetVersionRequest{Project: ref.GetProject(), Name: ref.GetName(), Version: ref.GetVersion()}))
	if err != nil {
		return nil, err
	}
	dataset := response.Msg.GetDataset()
	if dataset.GetSha256() != ref.GetSha256() {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("catalog pin differs from release policy"))
	}
	insufficient := func(reason string) {
		d.Outcome = pb.ReleaseOutcome_RELEASE_OUTCOME_INSUFFICIENT
		d.Reasons = append(d.Reasons, reason)
	}
	if dataset.GetContent().GetSplit() != "held-out" {
		insufficient("release decisions require a held-out dataset")
	}
	if dataset.GetState() != pb.DatasetVersionState_DATASET_VERSION_STATE_APPROVED {
		insufficient("dataset is not currently approved")
	}
	d.DatasetReviewedBy, d.DatasetReviewedAt = dataset.GetReviewedBy(), dataset.GetReviewedAt()
	if !proto.Equal(a.evidence.GetDataset(), ref) || !proto.Equal(b.evidence.GetDataset(), ref) {
		insufficient("baseline/candidate must use the policy's exact approved catalog version")
	}
	if a.run.GetStatus() != pb.RunStatus_RUN_STATUS_SUCCEEDED {
		insufficient("baseline is not a completed successful run")
	}
	if b.run.GetStatus() != pb.RunStatus_RUN_STATUS_SUCCEEDED && b.run.GetStatus() != pb.RunStatus_RUN_STATUS_FAILED {
		insufficient("candidate evaluation is not complete")
	}
	if a.evidence.GetEvaluationContractSha256() == "" || a.evidence.GetEvaluationContractSha256() != b.evidence.GetEvaluationContractSha256() {
		insufficient("captured evaluator, judge, summary, trial or checker contracts differ or are unavailable")
	}
	av, bv := snapshotValues(a, dataset), snapshotValues(b, dataset)
	for _, reason := range av.issues {
		insufficient("baseline: " + reason)
	}
	for _, reason := range bv.issues {
		insufficient("candidate: " + reason)
	}
	if d.GetOutcome() != pb.ReleaseOutcome_RELEASE_OUTCOME_INSUFFICIENT {
		for _, rule := range policy.GetRules() {
			metric := metricDecision(rule, policy, av, bv, a.records, trialsOf(a.run.GetSpec()))
			d.Metrics = append(d.Metrics, metric)
			if metric.GetOutcome() == pb.ReleaseOutcome_RELEASE_OUTCOME_INSUFFICIENT {
				d.Outcome = metric.GetOutcome()
			} else if metric.GetOutcome() == pb.ReleaseOutcome_RELEASE_OUTCOME_BLOCK && d.GetOutcome() == pb.ReleaseOutcome_RELEASE_OUTCOME_PASS {
				d.Outcome = metric.GetOutcome()
			}
		}
		if b.run.GetStatus() == pb.RunStatus_RUN_STATUS_FAILED {
			if d.GetOutcome() == pb.ReleaseOutcome_RELEASE_OUTCOME_PASS {
				d.Outcome = pb.ReleaseOutcome_RELEASE_OUTCOME_BLOCK
			}
			d.Reasons = append(d.Reasons, "candidate's existing run gates failed")
		}
	}
	d.Sha256 = store.ReleaseDecisionHash(d)
	access := authz.DatasetAccessSnapshot(dataset.GetContent())
	envelope := &pb.StoredReleaseDecision{Decision: d, BaselineAccess: authz.RunAccessSnapshot(a.run), CandidateAccess: authz.RunAccessSnapshot(b.run), DatasetAccess: access}
	if !ReleaseReadable(ctx, envelope) {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("release source read permissions are required"))
	}
	if proto.Size(envelope) > 4<<20 {
		return nil, connect.NewError(connect.CodeResourceExhausted, errors.New("release report exceeds 4 MiB"))
	}
	if err := m.store.CreateReleaseDecision(ctx, envelope); err != nil {
		return nil, releaseStoreError(err)
	}
	return connect.NewResponse(&pb.CreateReleaseDecisionResponse{Decision: d}), nil
}
func (m *Manager) GetReleaseDecision(ctx context.Context, req *connect.Request[pb.GetReleaseDecisionRequest]) (*connect.Response[pb.GetReleaseDecisionResponse], error) {
	d, err := m.store.GetReleaseDecision(ctx, projectOr(req.Msg.GetProject()), req.Msg.GetId())
	if err != nil {
		return nil, releaseStoreError(err)
	}
	if !ReleaseReadable(ctx, d) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("release decision not found"))
	}
	return connect.NewResponse(&pb.GetReleaseDecisionResponse{Decision: d.GetDecision()}), nil
}
func (m *Manager) ListReleaseDecisions(ctx context.Context, req *connect.Request[pb.ListReleaseDecisionsRequest]) (*connect.Response[pb.ListReleaseDecisionsResponse], error) {
	r := req.Msg
	size := int(r.GetPageSize())
	if size == 0 {
		size = 20
	}
	if size < 1 || size > 50 {
		return nil, invalid("page_size must be 1-50")
	}
	if r.GetPageToken() != "" && !datasets.ValidName(r.GetPageToken()) {
		return nil, invalid("invalid release cursor")
	}
	after := r.GetPageToken()
	out := []*pb.ReleaseDecision{}
	for len(out) <= size {
		rows, err := m.store.ListReleaseDecisions(ctx, projectOr(r.GetProject()), after, 50)
		if err != nil {
			return nil, releaseStoreError(err)
		}
		for _, d := range rows {
			after = d.GetDecision().GetId()
			if ReleaseReadable(ctx, d) {
				out = append(out, d.GetDecision())
				if len(out) > size {
					break
				}
			}
		}
		if len(out) > size || len(rows) < 50 {
			break
		}
	}
	next := ""
	if len(out) > size {
		out = out[:size]
		next = out[size-1].GetId()
	}
	for _, d := range out {
		d.Metrics = nil
		d.Policy.Rules = nil
	}
	return connect.NewResponse(&pb.ListReleaseDecisionsResponse{Decisions: out, NextPageToken: next}), nil
}

func (m *Manager) InspectReleasePolicy(ctx context.Context, req *connect.Request[pb.InspectReleasePolicyRequest]) (*connect.Response[pb.InspectReleasePolicyResponse], error) {
	policy := req.Msg.GetPolicy()
	if err := validateReleasePolicy(policy); err != nil {
		return nil, err
	}
	ref := policy.GetDataset()
	if _, err := datasetcatalog.New(m.store).GetDatasetVersion(ctx, connect.NewRequest(&pb.GetDatasetVersionRequest{Project: ref.GetProject(), Name: ref.GetName(), Version: ref.GetVersion()})); err != nil {
		return nil, err
	}
	return connect.NewResponse(&pb.InspectReleasePolicyResponse{Sha256: releaseHash("evalsi.release-policy/v1", policy)}), nil
}
