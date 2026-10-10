package webhooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/auth"
	"github.com/abhishek-rnjn/evals.si/internal/authz"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

func invalid(format string, args ...any) error {
	return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(format, args...))
}

func project(p string) string {
	if p == "" {
		return authz.DefaultProject
	}
	return p
}

func notFound(proj, name string) error {
	return connect.NewError(connect.CodeNotFound, fmt.Errorf("webhook %s/%s not found", proj, name))
}

// redacted is a webhook as callers see it: without its secret.
func redacted(w *evalsiv1alpha1.Webhook) *evalsiv1alpha1.Webhook {
	out := proto.Clone(w).(*evalsiv1alpha1.Webhook)
	out.SecretSet = out.Secret != ""
	out.Secret = ""
	return out
}

func validate(w *evalsiv1alpha1.Webhook) error {
	if !nameRE.MatchString(w.GetName()) {
		return invalid("webhook.name must be lowercase letters, digits, '.', '_' or '-' (at most 63 characters)")
	}
	u, err := url.Parse(w.GetUrl())
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return invalid("webhook.url must be an http or https URL")
	}
	if u.User != nil {
		return invalid("webhook.url must not carry credentials; the signature authenticates deliveries")
	}
	for _, e := range w.GetEvents() {
		if eventName(e) == "" {
			return invalid("webhook.events: unknown event %v", e)
		}
	}
	if sec := w.GetSecret(); sec != "" && len(sec) < 16 {
		return invalid("webhook.secret must be at least 16 characters (or leave it empty to have one generated)")
	}
	return nil
}

// ApplyWebhook creates or replaces a webhook.
func (s *Service) ApplyWebhook(ctx context.Context, req *connect.Request[evalsiv1alpha1.ApplyWebhookRequest]) (*connect.Response[evalsiv1alpha1.ApplyWebhookResponse], error) {
	w, _ := proto.Clone(req.Msg.GetWebhook()).(*evalsiv1alpha1.Webhook)
	if w == nil {
		return nil, invalid("webhook is required")
	}
	w.Project = project(w.GetProject())
	if err := validate(w); err != nil {
		return nil, err
	}
	// Events are kept sorted and unique so applying the same webhook twice is a no-op.
	slices.Sort(w.Events)
	w.Events = slices.Compact(w.Events)
	generated := false
	if w.GetSecret() == "" {
		old, err := s.store.GetWebhook(ctx, w.GetProject(), w.GetName())
		switch {
		case err == nil:
			w.Secret = old.GetSecret()
		case errors.Is(err, store.ErrNotFound):
			w.Secret, generated = newSecret(), true
		default:
			return nil, err
		}
	}
	w.UpdatedAt = timestamppb.New(s.opts.Now())
	if p := auth.PrincipalFrom(ctx); p != nil {
		w.UpdatedBy = p.ID()
	}
	if err := s.store.PutWebhook(ctx, w); err != nil {
		return nil, err
	}
	s.forgetTraceHooks(w.GetProject())
	out := redacted(w)
	if generated {
		out.Secret = w.GetSecret()
	}
	return connect.NewResponse(&evalsiv1alpha1.ApplyWebhookResponse{Webhook: out}), nil
}

// ListWebhooks lists a project's webhooks, without their secrets.
func (s *Service) ListWebhooks(ctx context.Context, req *connect.Request[evalsiv1alpha1.ListWebhooksRequest]) (*connect.Response[evalsiv1alpha1.ListWebhooksResponse], error) {
	ws, err := s.store.ListWebhooks(ctx, project(req.Msg.GetProject()))
	if err != nil {
		return nil, err
	}
	var visible []*evalsiv1alpha1.Webhook
	for _, w := range ws {
		// Each webhook is checked by its own name and labels.
		if authz.Can(ctx, "webhooks.read", w.GetProject(), authz.WebhookResource(w.GetName(), w.GetLabels())) {
			visible = append(visible, redacted(w))
		}
	}
	return connect.NewResponse(&evalsiv1alpha1.ListWebhooksResponse{Webhooks: visible}), nil
}

// GetWebhook returns a webhook, without its secret.
func (s *Service) GetWebhook(ctx context.Context, req *connect.Request[evalsiv1alpha1.GetWebhookRequest]) (*connect.Response[evalsiv1alpha1.GetWebhookResponse], error) {
	proj := project(req.Msg.GetProject())
	w, err := s.store.GetWebhook(ctx, proj, req.Msg.GetName())
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFound(proj, req.Msg.GetName())
	}
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&evalsiv1alpha1.GetWebhookResponse{Webhook: redacted(w)}), nil
}

// DeleteWebhook removes a webhook and its deliveries.
func (s *Service) DeleteWebhook(ctx context.Context, req *connect.Request[evalsiv1alpha1.DeleteWebhookRequest]) (*connect.Response[evalsiv1alpha1.DeleteWebhookResponse], error) {
	proj := project(req.Msg.GetProject())
	err := s.store.DeleteWebhook(ctx, proj, req.Msg.GetName())
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFound(proj, req.Msg.GetName())
	}
	if err != nil {
		return nil, err
	}
	s.forgetTraceHooks(proj)
	return connect.NewResponse(&evalsiv1alpha1.DeleteWebhookResponse{}), nil
}

// TestWebhook sends a ping now and reports the endpoint's answer.
func (s *Service) TestWebhook(ctx context.Context, req *connect.Request[evalsiv1alpha1.TestWebhookRequest]) (*connect.Response[evalsiv1alpha1.TestWebhookResponse], error) {
	proj := project(req.Msg.GetProject())
	w, err := s.store.GetWebhook(ctx, proj, req.Msg.GetName())
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFound(proj, req.Msg.GetName())
	}
	if err != nil {
		return nil, err
	}
	id := "whd_" + randomHex(12)
	body, err := json.Marshal(payload{ID: id, Type: EventPing, CreatedAt: s.opts.Now().UTC(), Project: proj})
	if err != nil {
		return nil, err
	}
	code, serr := s.send(ctx, w, id, EventPing, body)
	resp := &evalsiv1alpha1.TestWebhookResponse{Delivered: serr == nil, StatusCode: int32(code)}
	if serr != nil {
		resp.Error = serr.Error()
	}
	return connect.NewResponse(resp), nil
}

var deliveryStates = map[int]evalsiv1alpha1.DeliveryState{
	store.DeliveryPending:   evalsiv1alpha1.DeliveryState_DELIVERY_STATE_PENDING,
	store.DeliveryDelivered: evalsiv1alpha1.DeliveryState_DELIVERY_STATE_DELIVERED,
	store.DeliveryFailed:    evalsiv1alpha1.DeliveryState_DELIVERY_STATE_FAILED,
}

// ListWebhookDeliveries lists a webhook's latest deliveries.
func (s *Service) ListWebhookDeliveries(ctx context.Context, req *connect.Request[evalsiv1alpha1.ListWebhookDeliveriesRequest]) (*connect.Response[evalsiv1alpha1.ListWebhookDeliveriesResponse], error) {
	proj := project(req.Msg.GetProject())
	if _, err := s.store.GetWebhook(ctx, proj, req.Msg.GetName()); errors.Is(err, store.ErrNotFound) {
		return nil, notFound(proj, req.Msg.GetName())
	} else if err != nil {
		return nil, err
	}
	limit := int(req.Msg.GetLimit())
	if limit <= 0 {
		limit = 50
	}
	limit = min(limit, 200)
	ds, err := s.store.RecentDeliveries(ctx, proj, req.Msg.GetName(), limit)
	if err != nil {
		return nil, err
	}
	out := &evalsiv1alpha1.ListWebhookDeliveriesResponse{}
	for _, d := range ds {
		m := &evalsiv1alpha1.WebhookDelivery{
			Id: d.ID, Webhook: d.Webhook, Project: d.Project, Event: d.Event, RunId: d.RunID, TraceId: d.TraceID, Policy: d.Policy,
			State: deliveryStates[d.State], Attempts: int32(d.Attempts), StatusCode: int32(d.StatusCode), Error: d.Error,
			CreatedAt: timestamppb.New(d.Created),
		}
		if d.State == store.DeliveryPending {
			m.NextAttemptAt = timestamppb.New(d.Next)
		}
		if !d.Delivered.IsZero() {
			m.DeliveredAt = timestamppb.New(d.Delivered)
		}
		out.Deliveries = append(out.Deliveries, m)
	}
	return connect.NewResponse(out), nil
}
