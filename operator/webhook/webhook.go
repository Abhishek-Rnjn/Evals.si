// Package webhook holds the operator's admission webhooks: the mutating one
// records who created each evals.si resource (evals.si/created-by, from the
// authenticated admission request, which the creator cannot forge), and the
// validating one checks specs against the API's schema (and, with a Remote,
// against the API itself: credential grants, judges and access rules) and
// keeps EvalRun specs and the creator from changing.
package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"

	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	v1 "github.com/abhishek-rnjn/evals.si/operator/api/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/operator/spec"
)

// Paths the webhooks serve at.
const (
	MutatePath   = "/mutate-evals-si"
	ValidatePath = "/validate-evals-si"
)

// Mutator stamps the creator.
type Mutator struct{}

// +kubebuilder:webhook:path=/mutate-evals-si,mutating=true,failurePolicy=fail,sideEffects=None,groups=evals.si,resources=evalruns;onlineevalpolicies;evaluators;sandboxclasses,verbs=create;update,versions=v1alpha1,name=mutate.evals.si,admissionReviewVersions=v1

func (Mutator) Handle(_ context.Context, req admission.Request) admission.Response {
	obj, err := decode(req.Object.Raw)
	if err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	want := req.UserInfo.Username
	if req.Operation == admissionv1.Update {
		old, err := decode(req.OldObject.Raw)
		if err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		// Created before the webhook: nobody is recorded, nor may be.
		want = old.GetAnnotations()[v1.CreatedByAnnotation]
	}
	ann := obj.GetAnnotations()
	if got, ok := ann[v1.CreatedByAnnotation]; got == want && (ok || want == "") {
		return admission.Allowed("")
	}
	if ann == nil {
		ann = map[string]string{}
	}
	if want == "" {
		delete(ann, v1.CreatedByAnnotation)
	} else {
		ann[v1.CreatedByAnnotation] = want
	}
	obj.SetAnnotations(ann)
	patched, err := json.Marshal(obj.Object)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	return admission.PatchResponseFromRaw(req.Object.Raw, patched)
}

// Validator checks specs.
type Validator struct {
	// Asks the API whether it would accept the resource; nil checks the
	// schema only.
	Remote Remote
}

// Remote checks resources against the evalsi API without creating them.
type Remote interface {
	CheckRun(ctx context.Context, project, name string, spec *evalsiv1alpha1.RunSpec, labels map[string]string) error
	CheckPolicy(ctx context.Context, p *evalsiv1alpha1.OnlineEvalPolicy) error
}

// remoteTimeout bounds the API call inside an admission request.
const remoteTimeout = 5 * time.Second

// +kubebuilder:webhook:path=/validate-evals-si,mutating=false,failurePolicy=fail,sideEffects=None,groups=evals.si,resources=evalruns;onlineevalpolicies,verbs=create;update,versions=v1alpha1,name=validate.evals.si,admissionReviewVersions=v1

func (val Validator) Handle(ctx context.Context, req admission.Request) admission.Response {
	obj, err := decode(req.Object.Raw)
	if err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	raw, err := specOf(obj)
	if err != nil {
		return admission.Denied(err.Error())
	}
	if req.Operation == admissionv1.Update {
		old, err := decode(req.OldObject.Raw)
		if err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		if was := old.GetAnnotations()[v1.CreatedByAnnotation]; was != "" && obj.GetAnnotations()[v1.CreatedByAnnotation] != was {
			return admission.Denied(v1.CreatedByAnnotation + " cannot change")
		}
		if req.Kind.Kind == "EvalRun" {
			oldRaw, _ := specOf(old)
			if !jsonEqual(raw, oldRaw) {
				return admission.Denied("an EvalRun's spec cannot change; create a new EvalRun")
			}
			return admission.Allowed("")
		}
	}
	var check func(context.Context) error
	switch req.Kind.Kind {
	case "EvalRun":
		var s *evalsiv1alpha1.RunSpec
		if s, err = spec.Run(raw); err == nil && val.Remote != nil {
			check = func(ctx context.Context) error {
				return val.Remote.CheckRun(ctx, v1.ProjectOf(obj), obj.GetName(), s, v1.APILabels(obj))
			}
		}
	case "OnlineEvalPolicy":
		var p *evalsiv1alpha1.OnlineEvalPolicy
		if p, err = spec.Policy(raw); err == nil && val.Remote != nil {
			p.Name, p.Project, p.Labels = obj.GetName(), v1.ProjectOf(obj), v1.APILabels(obj)
			check = func(ctx context.Context) error { return val.Remote.CheckPolicy(ctx, p) }
		}
	}
	if err != nil {
		return admission.Denied(err.Error())
	}
	if check == nil {
		return admission.Allowed("")
	}
	ctx, cancel := context.WithTimeout(ctx, remoteTimeout)
	defer cancel()
	return remoteVerdict(check(ctx))
}

// remoteVerdict turns the API's answer into an admission response: what the
// API would refuse is denied now, at kubectl apply, instead of failing later
// in the resource's status. When the API cannot answer (down, slow) or the
// project does not exist yet (the controller creates it), the resource is
// allowed with a warning and the controller reports the outcome.
func remoteVerdict(err error) admission.Response {
	if err == nil {
		return admission.Allowed("")
	}
	switch code := connect.CodeOf(err); {
	case code == connect.CodePermissionDenied,
		code == connect.CodeInvalidArgument && !strings.Contains(err.Error(), "unknown project"):
		msg := err.Error()
		var ce *connect.Error
		if errors.As(err, &ce) {
			msg = ce.Message()
		}
		return admission.Denied("the evalsi API refuses it: " + msg)
	default:
		return admission.Allowed("").WithWarnings("not checked against the evalsi API: " + err.Error())
	}
}

func decode(raw []byte) (*unstructured.Unstructured, error) {
	obj := &unstructured.Unstructured{}
	if err := obj.UnmarshalJSON(raw); err != nil {
		return nil, err
	}
	return obj, nil
}

func specOf(obj *unstructured.Unstructured) ([]byte, error) {
	s, ok := obj.Object["spec"]
	if !ok {
		return nil, fmt.Errorf("spec is required")
	}
	return json.Marshal(s)
}

func jsonEqual(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return bytes.Equal(a, b)
	}
	ja, _ := json.Marshal(x)
	jb, _ := json.Marshal(y)
	return bytes.Equal(ja, jb)
}

// Register adds both webhooks to a server; remote, when not nil, checks
// EvalRuns and policies against the API before they are admitted.
func Register(s webhook.Server, remote Remote) {
	s.Register(MutatePath, &webhook.Admission{Handler: Mutator{}})
	s.Register(ValidatePath, &webhook.Admission{Handler: Validator{Remote: remote}})
}
