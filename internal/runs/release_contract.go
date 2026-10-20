package runs

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"hash"

	pb "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/evaluation"
	"github.com/abhishek-rnjn/evals.si/internal/version"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

func writeEvidence(h hash.Hash, data []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(data)))
	_, _ = h.Write(size[:])
	_, _ = h.Write(data)
}
func deterministic(message proto.Message) []byte {
	data, err := (proto.MarshalOptions{Deterministic: true}).Marshal(message)
	if err != nil {
		panic(err)
	} // Only validated stored messages reach this helper.
	return data
}
func releaseHash(domain string, message proto.Message) string {
	h := sha256.New()
	_, _ = h.Write([]byte(domain + "\x00"))
	_, _ = h.Write(deterministic(message))
	return hex.EncodeToString(h.Sum(nil))
}

func (m *Manager) evaluationContract(spec *pb.RunSpec, insts []evaluation.Instance) (*pb.RunEvaluationContract, error) {
	contract := &pb.RunEvaluationContract{ServerVersion: version.Version}
	settings, err := json.Marshal(struct {
		Trials  int
		Summary []byte
		Checker []byte
		Options any
	}{trialsOf(spec), deterministic(spec.GetSummary()), deterministic(spec.GetEnvironment().GetChecker()), m.opts.Evaluate})
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(settings)
	contract.SettingsSha256 = hex.EncodeToString(hash[:])
	for _, in := range insts {
		bound := &pb.BoundRunEvaluator{Name: in.Name, Manifest: proto.Clone(in.Manifest).(*pb.EvaluatorManifest), Judge: in.Judge}
		if in.Params != nil {
			bound.Params = proto.Clone(in.Params).(*structpb.Struct)
		}
		if in.Manifest.GetRequires().GetJudge() {
			if judge, ok := m.opts.Judges[in.Judge]; ok {
				data, err := json.Marshal(judge)
				if err != nil {
					return nil, err
				}
				sum := sha256.Sum256(data)
				bound.JudgeSettingsSha256 = hex.EncodeToString(sum[:])
			}
		}
		contract.Evaluators = append(contract.Evaluators, bound)
	}
	return contract, nil
}
