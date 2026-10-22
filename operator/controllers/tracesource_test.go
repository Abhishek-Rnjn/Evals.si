package controllers

import (
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1/evalsiv1alpha1connect"
	v1 "github.com/abhishek-rnjn/evals.si/operator/api/v1alpha1"
)

func TestTraceSourceProjectMoveDeletesFromOldProject(t *testing.T) {
	f := &fakeAPI{runs: map[string]*evalsiv1alpha1.Run{}, policies: map[string]*evalsiv1alpha1.OnlineEvalPolicy{}, sources: map[string]*evalsiv1alpha1.TraceSource{},
		projects: map[string]bool{"a": true, "b": true}}
	mux := http.NewServeMux()
	mux.Handle(evalsiv1alpha1connect.NewSourceServiceHandler(f))
	srv := httptest.NewServer(mux)
	defer srv.Close()
	api, _ := NewAPI(APIConfig{URL: srv.URL})
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1.AddToScheme(scheme)
	ts := &v1.TraceSource{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "evalsi", Labels: map[string]string{v1.ProjectLabel: "a"}, Generation: 1},
		Spec: runtime.RawExtension{Raw: []byte(`{"connector":"mlflow","endpoint":"http://x.example.com"}`)}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ts).WithStatusSubresource(ts).Build()
	r := &TraceSourceReconciler{Client: c, API: api, Projects: v1.ProjectPolicy{Home: "evalsi"}}
	ctx := context.Background()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "evalsi", Name: "s"}}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	_ = c.Get(ctx, req.NamespacedName, ts)
	ts.Labels[v1.ProjectLabel] = "b"
	_ = c.Update(ctx, ts)
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if len(f.sources) != 1 {
		t.Errorf("sources after label move: %v", slices.Collect(maps.Keys(f.sources)))
	}
}
