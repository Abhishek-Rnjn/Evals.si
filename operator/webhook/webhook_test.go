package webhook

import (
	"context"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	v1 "github.com/abhishek-rnjn/evals.si/operator/api/v1alpha1"
)

// The operator removes its finalizer with an Update of a deleting object.
// If the project mapping was removed since creation, the validator denies it.
func TestUpdateOfDeletingObjectAllowed(t *testing.T) {
	raw := func(del bool) []byte {
		s := `{"apiVersion":"evals.si/v1alpha1","kind":"OnlineEvalPolicy","metadata":{"name":"p","namespace":"team-a","labels":{"evals.si/project":"shared"}`
		if del {
			s += `,"deletionTimestamp":"2026-01-01T00:00:00Z","finalizers":[]`
		} else {
			s += `,"finalizers":["evals.si/finalizer"]`
		}
		return []byte(s + `},"spec":{"sampling":{"rate":1}}}`)
	}
	val := Validator{Projects: v1.ProjectPolicy{Home: "evalsi"}} // mapping for "shared" dropped
	resp := val.Handle(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Update, Kind: metav1.GroupVersionKind{Kind: "OnlineEvalPolicy"},
		Object: runtime.RawExtension{Raw: raw(true)}, OldObject: runtime.RawExtension{Raw: raw(false)},
	}})
	if !resp.Allowed {
		t.Errorf("finalizer removal on a deleting object denied: %v", resp.Result.Message)
	}
}
