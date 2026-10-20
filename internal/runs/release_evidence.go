package runs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"

	"connectrpc.com/connect"
	pb "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/datasetcatalog"
	"github.com/abhishek-rnjn/evals.si/internal/store"
	"google.golang.org/protobuf/proto"
)

type releaseSnapshot struct {
	run      *pb.Run
	records  []*pb.Record
	outputs  map[[2]int]store.Output
	results  []store.Result
	evidence *pb.ReleaseRunEvidence
}

func (m *Manager) releaseSnapshot(ctx context.Context, id string) (*releaseSnapshot, error) {
	run, err := m.get(ctx, id)
	if err != nil {
		return nil, err
	}
	if run.GetRecords() > maxReleaseCells || int64(trialsOf(run.GetSpec()))*run.GetRecords() > maxReleaseCells || int64(max(1, len(run.GetEvaluationContract().GetEvaluators())))*int64(trialsOf(run.GetSpec()))*run.GetRecords() > maxReleaseCells {
		return nil, connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("release evidence exceeds 100000 record/trial/evaluator cells"))
	}
	s := &releaseSnapshot{run: run}
	if ref := run.GetSpec().GetDataset().GetCatalog(); ref != nil {
		if _, err := datasetcatalog.New(m.store).GetDatasetVersion(ctx, connect.NewRequest(&pb.GetDatasetVersionRequest{Project: datasetProject(run), Name: ref.GetName(), Version: ref.GetVersion()})); err != nil {
			return nil, err
		}
	}
	s.records, err = m.store.Records(ctx, id)
	if err != nil {
		return nil, err
	}
	s.outputs, err = m.store.Outputs(ctx, id)
	if err != nil {
		return nil, err
	}
	s.results, err = m.store.Results(ctx, id, "", maxReleaseCells+1, 0)
	if err != nil {
		return nil, err
	}
	if len(s.results) > maxReleaseCells {
		return nil, connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("release result count exceeds 100000"))
	}
	h := sha256.New()
	_, _ = h.Write([]byte("evalsi.release-run/v1\x00"))
	writeEvidence(h, deterministic(run))
	for _, r := range s.records {
		writeEvidence(h, deterministic(r))
	}
	var keys [][2]int
	for key := range s.outputs {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})
	for _, key := range keys {
		o := s.outputs[key]
		writeEvidence(h, []byte(fmt.Sprintf("output:%d:%d", key[0], key[1])))
		writeEvidence(h, deterministic(o.Record))
		writeEvidence(h, []byte(o.Error))
	}
	for _, row := range s.results {
		writeEvidence(h, []byte(fmt.Sprintf("result:%d:%d:%d", row.RecordIdx, row.Trial, row.EvalIdx)))
		writeEvidence(h, deterministic(row.Result))
	}
	current, err := m.store.GetRun(ctx, id)
	if err != nil || !proto.Equal(current, run) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("run changed while evidence was read; reload"))
	}
	e := &pb.ReleaseRunEvidence{Reference: &pb.ReleaseRunEvidenceRef{RunId: id, Sha256: hex.EncodeToString(h.Sum(nil)), Commit: run.GetLabels()["agent.commit"]}, Project: run.GetProject(), Status: run.GetStatus(), Records: run.GetRecords(), Trials: int32(trialsOf(run.GetSpec())), Imported: run.GetImported(), Archived: run.GetArchivedAt() != nil, SpecSha256: releaseHash("evalsi.run-spec/v1", run.GetSpec())}
	if run.GetEvaluationContract() != nil {
		e.EvaluationContractSha256 = releaseHash("evalsi.evaluation-contract/v1", run.GetEvaluationContract())
	}
	if ref := run.GetSpec().GetDataset().GetCatalog(); ref != nil {
		e.Dataset = proto.Clone(ref).(*pb.DatasetVersionRef)
		e.Dataset.Project = datasetProject(run)
	}
	e.ServerVersion = run.GetEvaluationContract().GetServerVersion()
	e.ModelConnector, e.Model = run.GetSpec().GetTarget().GetConnector(), run.GetSpec().GetTarget().GetModel()
	e.ModelSettingsSha256 = releaseHash("evalsi.target-settings/v1", run.GetSpec().GetTarget())
	e.EnvironmentImage = run.GetSpec().GetEnvironment().GetImage()
	e.EnvironmentSha256 = releaseHash("evalsi.environment/v1", run.GetSpec().GetEnvironment())
	e.CheckerSha256 = releaseHash("evalsi.checker/v1", run.GetSpec().GetEnvironment().GetChecker())
	if agent := run.GetSpec().GetTarget().GetAgent(); agent != nil {
		reflection := agent.ProtoReflect()
		if field := reflection.WhichOneof(reflection.Descriptor().Oneofs().ByName("kind")); field != nil {
			e.AgentKind = string(field.Name())
		}
	}
	for _, bound := range run.GetEvaluationContract().GetEvaluators() {
		e.Evaluators = append(e.Evaluators, &pb.ReleaseEvaluatorIdentity{Name: bound.GetName(), Ref: bound.GetManifest().GetName() + "@" + bound.GetManifest().GetVersion(), ParamsSha256: releaseHash("evalsi.evaluator-params/v1", bound.GetParams()), Judge: bound.GetJudge(), JudgeSettingsSha256: bound.GetJudgeSettingsSha256(), Scope: bound.GetManifest().GetScope()})
	}
	s.evidence = e
	return s, nil
}
func datasetProject(run *pb.Run) string {
	if project := run.GetSpec().GetDataset().GetCatalog().GetProject(); project != "" {
		return project
	}
	return projectOr(run.GetProject())
}
func (m *Manager) GetReleaseRunEvidence(ctx context.Context, req *connect.Request[pb.GetReleaseRunEvidenceRequest]) (*connect.Response[pb.GetReleaseRunEvidenceResponse], error) {
	s, err := m.releaseSnapshot(ctx, req.Msg.GetRunId())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&pb.GetReleaseRunEvidenceResponse{Evidence: s.evidence}), nil
}
