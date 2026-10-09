package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"connectrpc.com/connect"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1/evalsiv1alpha1connect"
	"github.com/abhishek-rnjn/evals.si/internal/auth"
	"github.com/abhishek-rnjn/evals.si/internal/authz"
	"github.com/abhishek-rnjn/evals.si/internal/config"
)

func TestAnnotationQueues(t *testing.T) {
	annotators := map[string]string{}
	s := startAuthServer(t, func(c *config.Config) {
		for _, name := range []string{"ann1", "ann2"} {
			plain, hash := auth.NewAPIKey()
			annotators[name] = plain
			c.Auth.APIKeys.Keys = append(c.Auth.APIKeys.Keys, auth.ConfigKey{
				Name: name, Key: "sha256:" + hash, Roles: map[string][]string{"support": {"annotator"}},
			})
		}
	})
	ctx := context.Background()
	client := func(cred string) evalsiv1alpha1connect.AnnotationServiceClient {
		return evalsiv1alpha1connect.NewAnnotationServiceClient(s.http, s.url, as(cred))
	}

	// A run whose records the queue will hold.
	runs := evalsiv1alpha1connect.NewRunServiceClient(s.http, s.url, as(s.keys["owner"]))
	spec := runSpec("m")
	var recs []*evalsiv1alpha1.Record
	for _, id := range []string{"r1", "r2", "r3", "r4"} {
		recs = append(recs, &evalsiv1alpha1.Record{Id: id, Input: text("Capital of France?"), Reference: text("Paris")})
	}
	spec.Dataset = &evalsiv1alpha1.DatasetSource{Source: &evalsiv1alpha1.DatasetSource_Inline{Inline: &evalsiv1alpha1.InlineRecords{Records: recs}}}
	created, err := runs.CreateRun(ctx, connect.NewRequest(&evalsiv1alpha1.CreateRunRequest{Project: "support", Name: "judged", Spec: spec}))
	if err != nil {
		t.Fatal(err)
	}
	runID := created.Msg.GetRun().GetId()
	deadline := time.Now().Add(20 * time.Second)
	for {
		got, err := runs.GetRun(ctx, connect.NewRequest(&evalsiv1alpha1.GetRunRequest{Id: runID}))
		if err != nil {
			t.Fatal(err)
		}
		if got.Msg.GetRun().GetStatus() == evalsiv1alpha1.RunStatus_RUN_STATUS_SUCCEEDED {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %v", got.Msg.GetRun())
		}
		time.Sleep(50 * time.Millisecond)
	}

	queue := &evalsiv1alpha1.AnnotationQueue{
		Name: "helpfulness", Project: "support", AnnotationsPerItem: 2,
		Questions: []*evalsiv1alpha1.Question{
			{Name: "correct", Kind: evalsiv1alpha1.QuestionKind_QUESTION_KIND_PASS_FAIL, CompareMetric: "exact-match"},
			{Name: "quality", Kind: evalsiv1alpha1.QuestionKind_QUESTION_KIND_SCORE, Min: 1, Max: 5},
			{Name: "tone", Kind: evalsiv1alpha1.QuestionKind_QUESTION_KIND_LABEL, Options: []string{"polite", "rude"}},
			{Name: "notes", Kind: evalsiv1alpha1.QuestionKind_QUESTION_KIND_TEXT, Optional: true},
		},
	}
	// Annotators answer; they do not set queues up.
	if _, err := client(annotators["ann1"]).CreateQueue(ctx, connect.NewRequest(&evalsiv1alpha1.CreateQueueRequest{Queue: queue})); codeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("annotator creating a queue: %v", err)
	}
	editor := client(s.keys["editor"])
	if _, err := editor.CreateQueue(ctx, connect.NewRequest(&evalsiv1alpha1.CreateQueueRequest{Queue: queue})); err != nil {
		t.Fatal(err)
	}
	if _, err := editor.CreateQueue(ctx, connect.NewRequest(&evalsiv1alpha1.CreateQueueRequest{Queue: queue})); codeOf(err) != connect.CodeAlreadyExists {
		t.Errorf("duplicate queue: %v", err)
	}
	bad := &evalsiv1alpha1.AnnotationQueue{Name: "bad", Project: "support", Questions: []*evalsiv1alpha1.Question{{Name: "q", Kind: evalsiv1alpha1.QuestionKind_QUESTION_KIND_LABEL, Options: []string{"only"}}}}
	if _, err := editor.CreateQueue(ctx, connect.NewRequest(&evalsiv1alpha1.CreateQueueRequest{Queue: bad})); codeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("invalid rubric: %v", err)
	}

	addReq := &evalsiv1alpha1.AddItemsRequest{Project: "support", Queue: "helpfulness",
		Source: &evalsiv1alpha1.AddItemsRequest_Run{Run: &evalsiv1alpha1.AddItemsFromRun{RunId: runID, When: `scores["exact-match"] >= 0`}}}
	if _, err := client(annotators["ann1"]).AddItems(ctx, connect.NewRequest(addReq)); codeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("annotator adding items: %v", err)
	}
	added, err := editor.AddItems(ctx, connect.NewRequest(addReq))
	if err != nil || added.Msg.GetAdded() != 4 {
		t.Fatalf("add: %v %v", added, err)
	}

	answers := func(correct bool, quality float64, tone string) []*evalsiv1alpha1.Answer {
		return []*evalsiv1alpha1.Answer{
			{Question: "correct", Value: &evalsiv1alpha1.Answer_Passed{Passed: correct}},
			{Question: "quality", Value: &evalsiv1alpha1.Answer_Number{Number: quality}},
			{Question: "tone", Value: &evalsiv1alpha1.Answer_Label{Label: tone}},
		}
	}
	// An item needing two answers is offered to two annotators at once; a
	// third gets the next one.
	next := func(cred string) *evalsiv1alpha1.AnnotationItem {
		t.Helper()
		resp, err := client(cred).NextItem(ctx, connect.NewRequest(&evalsiv1alpha1.NextItemRequest{Project: "support", Queue: "helpfulness"}))
		if err != nil {
			t.Fatal(err)
		}
		return resp.Msg.GetItem()
	}
	first, second, third := next(annotators["ann1"]), next(annotators["ann2"]), next(s.keys["editor"])
	if first.GetId() == "" || first.GetId() != second.GetId() || third.GetId() == first.GetId() {
		t.Fatalf("claims: %v / %v / %v", first.GetId(), second.GetId(), third.GetId())
	}
	if again := next(annotators["ann1"]); again.GetId() != first.GetId() {
		t.Errorf("a held item comes back first: %v", again.GetId())
	}
	if first.GetRecord().GetOutput().GetText() != "Paris" || first.GetRunScores()["exact-match"] != 1 || first.GetSource().GetRunId() != runID {
		t.Errorf("item: %v", first)
	}
	// The rubric is enforced.
	submit := func(cred, item string, ans []*evalsiv1alpha1.Answer) error {
		_, err := client(cred).SubmitAnnotation(ctx, connect.NewRequest(&evalsiv1alpha1.SubmitAnnotationRequest{
			Project: "support", Queue: "helpfulness", ItemId: item, Answers: ans}))
		return err
	}
	if err := submit(annotators["ann1"], first.GetId(), answers(true, 9, "polite")); codeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("score out of range: %v", err)
	}
	if err := submit(annotators["ann1"], first.GetId(), answers(true, 4, "polite")[:2]); codeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("missing a required answer: %v", err)
	}
	if err := submit(s.keys["viewer"], first.GetId(), answers(true, 4, "polite")); codeOf(err) != connect.CodePermissionDenied {
		t.Errorf("viewer submitting: %v", err)
	}

	// Skipping releases the claim without counting as an answer.
	if _, err := client(s.keys["editor"]).SubmitAnnotation(ctx, connect.NewRequest(&evalsiv1alpha1.SubmitAnnotationRequest{
		Project: "support", Queue: "helpfulness", ItemId: third.GetId(), Skip: true})); err != nil {
		t.Fatal(err)
	}

	// Each annotator answers every item. ann1 says the last item is wrong;
	// ann2 agrees except on quality.
	for _, who := range []string{"ann1", "ann2"} {
		for i := 0; ; i++ {
			it := next(annotators[who])
			if it == nil {
				if i != 4 {
					t.Fatalf("%s answered %d items", who, i)
				}
				break
			}
			id := it.GetRecord().GetId()
			quality := 4.0
			if who == "ann2" {
				quality = 5
			}
			if err := submit(annotators[who], it.GetId(), answers(id != "r4", quality, "polite")); err != nil {
				t.Fatal(err)
			}
		}
	}

	// Anyone who may read the queue sees the statistics.
	stats, err := client(s.keys["viewer"]).SummarizeQueue(ctx, connect.NewRequest(&evalsiv1alpha1.SummarizeQueueRequest{Project: "support", Queue: "helpfulness"}))
	if err != nil {
		t.Fatal(err)
	}
	st := stats.Msg
	if st.GetItems() != 4 || st.GetDone() != 4 || st.GetAnnotations() != 8 || st.GetSkipped() != 1 || st.GetAnnotators()["key:ann1"] != 4 {
		t.Fatalf("stats: %v", st)
	}
	byName := map[string]*evalsiv1alpha1.QuestionStats{}
	for _, q := range st.GetQuestions() {
		byName[q.GetQuestion()] = q
	}
	correct := byName["correct"]
	if correct.GetSummary().GetMean() != 0.75 || correct.GetSummary().GetCi() == nil {
		t.Errorf("correct: %v", correct)
	}
	// The run's exact-match passed all four; the humans failed one: 3/4 agree.
	if ag := correct.GetMetricAgreement(); ag.GetN() != 4 || ag.GetAccuracy() != 0.75 {
		t.Errorf("agreement with exact-match: %v", ag)
	}
	if correct.GetInterAnnotatorAlpha() != 1 || correct.GetMultiplyAnnotated() != 4 {
		t.Errorf("inter-annotator: %v", correct)
	}
	if byName["quality"].GetSummary().GetMean() != 4.5 {
		t.Errorf("quality: %v", byName["quality"])
	}
	// Its interval stays on the question's 1-5 scale.
	if ci := byName["quality"].GetSummary().GetCi(); ci == nil || ci.GetLow() < 1 || ci.GetHigh() > 5 {
		t.Errorf("quality interval off the scale: %v", ci)
	}
	if byName["tone"].GetLabelCounts()["polite"] != 8 {
		t.Errorf("tone: %v", byName["tone"])
	}

	// Another project's members see nothing of it.
	if _, err := client(s.keys["checkout-viewer"]).GetQueue(ctx, connect.NewRequest(&evalsiv1alpha1.GetQueueRequest{Project: "support", Name: "helpfulness"})); codeOf(err) != connect.CodePermissionDenied {
		t.Errorf("other project: %v", err)
	}
	listed, err := client(s.keys["viewer"]).ListAnnotations(ctx, connect.NewRequest(&evalsiv1alpha1.ListAnnotationsRequest{Project: "support", Queue: "helpfulness", PageSize: 5}))
	if err != nil || len(listed.Msg.GetAnnotations()) != 5 || listed.Msg.GetNextPageToken() == "" || len(listed.Msg.GetItems()) == 0 {
		t.Fatalf("list: %v %v", listed, err)
	}

	// REST.
	req, _ := http.NewRequest(http.MethodGet, s.url+"/v1alpha1/queues/helpfulness/stats?project=support", nil)
	req.Header.Set("Authorization", "Bearer "+s.keys["viewer"])
	resp, err := s.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if resp.StatusCode != http.StatusOK || body["done"] != "4" {
		t.Errorf("REST stats: %d %v", resp.StatusCode, body)
	}
}

// TestQueueAndGuardrailRulesSeeStoredLabels: rules on a queue's or a
// guardrail's name or labels apply to every call on it, listing included,
// with the labels it was stored with.
func TestQueueAndGuardrailRulesSeeStoredLabels(t *testing.T) {
	s := startAuthServer(t, func(c *config.Config) {
		c.Authorization.Rules = append(c.Authorization.Rules,
			authz.Rule{Deny: `request.action in ["annotations.read", "annotations.write"] && resource.labels.exists(k, k == "confidential")`},
			authz.Rule{Deny: `request.action == "annotations.read" && has(resource.queue) && resource.queue.name == "sensitive"`},
			authz.Rule{Deny: `request.action.startsWith("guardrails.") && request.action != "guardrails.write" && resource.labels.exists(k, k == "confidential")`},
		)
	})
	ctx := context.Background()
	editor := evalsiv1alpha1connect.NewAnnotationServiceClient(s.http, s.url, as(s.keys["editor"]))
	question := []*evalsiv1alpha1.Question{{Name: "ok", Kind: evalsiv1alpha1.QuestionKind_QUESTION_KIND_PASS_FAIL}}
	for _, q := range []*evalsiv1alpha1.AnnotationQueue{
		{Name: "open", Project: "support", Questions: question},
		{Name: "sensitive", Project: "support", Questions: question},
		{Name: "hr", Project: "support", Questions: question, Labels: map[string]string{"confidential": "yes"}},
	} {
		if _, err := editor.CreateQueue(ctx, connect.NewRequest(&evalsiv1alpha1.CreateQueueRequest{Queue: q})); err != nil {
			t.Fatalf("%s: %v", q.GetName(), err)
		}
	}

	list, err := editor.ListQueues(ctx, connect.NewRequest(&evalsiv1alpha1.ListQueuesRequest{Project: "support"}))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, q := range list.Msg.GetQueues() {
		names = append(names, q.GetName())
	}
	if len(names) != 1 || names[0] != "open" {
		t.Errorf("listed: %v", names)
	}
	// The stored label applies to calls that do not carry it.
	for name, call := range map[string]func() error{
		"get": func() error {
			_, err := editor.GetQueue(ctx, connect.NewRequest(&evalsiv1alpha1.GetQueueRequest{Project: "support", Name: "hr"}))
			return err
		},
		"next": func() error {
			_, err := editor.NextItem(ctx, connect.NewRequest(&evalsiv1alpha1.NextItemRequest{Project: "support", Queue: "hr"}))
			return err
		},
		"stats": func() error {
			_, err := editor.SummarizeQueue(ctx, connect.NewRequest(&evalsiv1alpha1.SummarizeQueueRequest{Project: "support", Queue: "hr"}))
			return err
		},
		"answer": func() error {
			_, err := editor.SubmitAnnotation(ctx, connect.NewRequest(&evalsiv1alpha1.SubmitAnnotationRequest{Project: "support", Queue: "hr", ItemId: "x"}))
			return err
		},
	} {
		if err := call(); codeOf(err) != connect.CodePermissionDenied {
			t.Errorf("%s on the confidential queue: %v", name, err)
		}
	}

	// Guardrails: listing and checking see the stored label too.
	gr := evalsiv1alpha1connect.NewGuardrailServiceClient(s.http, s.url, as(s.keys["editor"]))
	email := []*evalsiv1alpha1.RedactRule{{Builtin: "email"}}
	for _, g := range []*evalsiv1alpha1.Guardrail{
		{Name: "public", Project: "support", Redact: email},
		{Name: "secret", Project: "support", Redact: email, Labels: map[string]string{"confidential": "yes"}},
	} {
		if _, err := gr.ApplyGuardrail(ctx, connect.NewRequest(&evalsiv1alpha1.ApplyGuardrailRequest{Guardrail: g})); err != nil {
			t.Fatal(err)
		}
	}
	gl, err := gr.ListGuardrails(ctx, connect.NewRequest(&evalsiv1alpha1.ListGuardrailsRequest{Project: "support"}))
	if err != nil || len(gl.Msg.GetGuardrails()) != 1 || gl.Msg.GetGuardrails()[0].GetName() != "public" {
		t.Errorf("guardrails listed: %v %v", gl, err)
	}
	if _, err := gr.Check(ctx, connect.NewRequest(&evalsiv1alpha1.CheckRequest{Project: "support", Guardrail: "secret", Content: "x"})); codeOf(err) != connect.CodePermissionDenied {
		t.Errorf("check on the confidential guardrail: %v", err)
	}
}
