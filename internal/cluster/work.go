package cluster

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"

	harnessv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/harness/v1alpha1"
	pluginv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/plugin/v1alpha1"
	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/pluginhost"
)

// Methods carried over the work queue.
const (
	mDescribe    = "Describe"
	mEvaluate    = "Evaluate"
	mReduce      = "Reduce"
	mGenerate    = "Generate"
	mLoadDataset = "LoadDataset"
	mRunTask     = "RunTask"

	kindResult = "result"
	kindEvent  = "event"
	kindError  = "error"
)

// Worker sends worker calls to the pools' queues. It implements
// pluginhost.Worker, so the runs and evaluation services use it unchanged.
type Worker struct {
	c *Cluster
	// Routing for Evaluate and Reduce: the evaluator's manifest decides the
	// pool (judge evaluators to judge, sandboxed ones to sandbox).
	mu     sync.Mutex
	routes map[string]string
}

// NewWorker returns the queue-backed worker.
func NewWorker(c *Cluster) *Worker { return &Worker{c: c, routes: map[string]string{}} }

func poolFor(m *evalsiv1alpha1.EvaluatorManifest) string {
	r := m.GetRequires()
	switch {
	case r.GetJudge():
		return "judge"
	case r.GetIsolation() > evalsiv1alpha1.IsolationLevel_ISOLATION_LEVEL_NONE:
		return "sandbox"
	}
	return "cpu"
}

func (w *Worker) route(evaluator string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if pool, ok := w.routes[evaluator]; ok {
		return pool
	}
	return "cpu"
}

// call sends one request to a pool and waits for its answer, passing stream
// events to onEvent.
func (w *Worker) call(ctx context.Context, pool, method string, req proto.Message, resp proto.Message, onEvent func([]byte) error) error {
	c := w.c
	inbox := c.nc.NewInbox()
	replies := make(chan *nats.Msg, 256)
	sub, err := c.nc.ChanSubscribe(inbox, replies)
	if err != nil {
		return err
	}
	defer func() { _ = sub.Unsubscribe() }()
	body, err := proto.Marshal(req)
	if err != nil {
		return err
	}
	msg := nats.NewMsg(workPrefix + pool)
	msg.Header.Set(hdrMethod, method)
	msg.Header.Set(hdrReply, inbox)
	if err := c.putPayload(ctx, msg, body); err != nil {
		return err
	}
	if _, err := c.js.PublishMsg(ctx, msg); err != nil {
		c.dropPayload(msg.Header)
		return connect.NewError(connect.CodeUnavailable, fmt.Errorf("queueing %s on the %s pool: %w", method, pool, err))
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case m := <-replies:
			data, err := c.payload(ctx, m.Header, m.Data)
			if err != nil {
				return err
			}
			c.dropPayload(m.Header)
			switch m.Header.Get(hdrKind) {
			case kindEvent:
				if onEvent != nil {
					if err := onEvent(data); err != nil {
						return err
					}
				}
			case kindError:
				code, _ := strconv.Atoi(m.Header.Get(hdrCode))
				return connect.NewError(connect.Code(code), errors.New(string(data)))
			default:
				return proto.Unmarshal(data, resp)
			}
		}
	}
}

func (w *Worker) Describe(ctx context.Context) ([]*evalsiv1alpha1.EvaluatorManifest, error) {
	resp := &pluginv1alpha1.DescribeResponse{}
	if err := w.call(ctx, "cpu", mDescribe, &pluginv1alpha1.DescribeRequest{}, resp, nil); err != nil {
		return nil, err
	}
	w.mu.Lock()
	for _, m := range resp.GetEvaluators() {
		pool := poolFor(m)
		w.routes[m.GetName()] = pool
		if short := shortName(m.GetName()); short != "" {
			w.routes[short] = pool
		}
	}
	w.mu.Unlock()
	return resp.GetEvaluators(), nil
}

// shortName is the name without its namespace ("builtin/exact-match" -> "exact-match").
func shortName(name string) string {
	for i := len(name) - 1; i >= 0; i-- {
		if name[i] == '/' {
			return name[i+1:]
		}
	}
	return ""
}

func (w *Worker) Evaluate(ctx context.Context, req *pluginv1alpha1.EvaluateRequest) (*pluginv1alpha1.EvaluateResponse, error) {
	resp := &pluginv1alpha1.EvaluateResponse{}
	return resp, w.call(ctx, w.route(req.GetEvaluator()), mEvaluate, req, resp, nil)
}

func (w *Worker) Reduce(ctx context.Context, req *pluginv1alpha1.ReduceRequest) (*pluginv1alpha1.ReduceResponse, error) {
	resp := &pluginv1alpha1.ReduceResponse{}
	return resp, w.call(ctx, w.route(req.GetEvaluator()), mReduce, req, resp, nil)
}

func (w *Worker) Generate(ctx context.Context, req *pluginv1alpha1.GenerateRequest) (*pluginv1alpha1.GenerateResponse, error) {
	resp := &pluginv1alpha1.GenerateResponse{}
	return resp, w.call(ctx, "judge", mGenerate, req, resp, nil)
}

func (w *Worker) LoadDataset(ctx context.Context, req *pluginv1alpha1.LoadDatasetRequest) ([]*evalsiv1alpha1.Record, error) {
	resp := &pluginv1alpha1.LoadDatasetResponse{}
	if err := w.call(ctx, "cpu", mLoadDataset, req, resp, nil); err != nil {
		return nil, err
	}
	return resp.GetRecords(), nil
}

func (w *Worker) RunTask(ctx context.Context, req *pluginv1alpha1.RunTaskRequest, onEvent func(*harnessv1alpha1.TrajectoryEvent)) (*pluginv1alpha1.TaskResult, error) {
	resp := &pluginv1alpha1.TaskResult{}
	err := w.call(ctx, "harness", mRunTask, req, resp, func(data []byte) error {
		ev := &harnessv1alpha1.TrajectoryEvent{}
		if err := proto.Unmarshal(data, ev); err != nil {
			return err
		}
		if onEvent != nil {
			onEvent(ev)
		}
		return nil
	})
	return resp, err
}

var _ pluginhost.Worker = (*Worker)(nil)

// Serve pulls tasks for the given pools and runs them on local, at most
// concurrency at a time per pool, until ctx ends. In-flight tasks finish
// (their messages are acknowledged) before it returns.
func (c *Cluster) Serve(ctx context.Context, pools []string, local pluginhost.Worker, concurrency int, log *slog.Logger) error {
	if concurrency < 1 {
		concurrency = 4
	}
	var wg sync.WaitGroup
	errs := make(chan error, len(pools))
	for _, pool := range pools {
		cons, err := c.js.Consumer(ctx, workStream, "pool-"+pool)
		if err != nil {
			return fmt.Errorf("cluster: %s pool: %w", pool, err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- c.servePool(ctx, pool, cons, local, concurrency, log)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
	}
	return nil
}

func (c *Cluster) servePool(ctx context.Context, pool string, cons jetstream.Consumer, local pluginhost.Worker, concurrency int, log *slog.Logger) error {
	slots := make(chan struct{}, concurrency)
	var tasks sync.WaitGroup
	defer tasks.Wait()
	for ctx.Err() == nil {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			return nil
		}
		batch, err := cons.Fetch(1, jetstream.FetchMaxWait(2*time.Second))
		if err != nil {
			<-slots
			if ctx.Err() != nil {
				return nil
			}
			log.Warn("fetching work", "pool", pool, "err", err)
			time.Sleep(time.Second)
			continue
		}
		got := false
		for msg := range batch.Messages() {
			got = true
			tasks.Add(1)
			go func() {
				defer tasks.Done()
				defer func() { <-slots }()
				c.handle(msg, local, log)
			}()
		}
		if !got {
			<-slots
		}
	}
	return nil
}

// handle runs one task. Its context is not the server's: a task that has
// started finishes even while the worker drains for shutdown.
func (c *Cluster) handle(msg jetstream.Msg, local pluginhost.Worker, log *slog.Logger) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { // heartbeat, so long tasks are not redelivered
		t := time.NewTicker(c.ackWait / 3)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				_ = msg.InProgress()
			}
		}
	}()
	h := msg.Headers()
	reply, method := h.Get(hdrReply), h.Get(hdrMethod)
	data, err := c.payload(ctx, nats.Header(h), msg.Data())
	if err == nil {
		var resp proto.Message
		resp, err = c.dispatch(ctx, method, data, local, func(ev proto.Message) {
			_ = c.reply(ctx, reply, kindEvent, ev, nil)
		})
		if err == nil {
			err = c.reply(ctx, reply, kindResult, resp, nil)
		}
	}
	if err != nil {
		log.Warn("worker task failed", "method", method, "err", err)
		if rerr := c.reply(ctx, reply, kindError, nil, err); rerr != nil {
			log.Warn("answering a task", "err", rerr)
		}
	}
	c.dropPayload(nats.Header(h))
	_ = msg.Ack()
}

func (c *Cluster) reply(ctx context.Context, subject, kind string, body proto.Message, callErr error) error {
	m := nats.NewMsg(subject)
	m.Header.Set(hdrKind, kind)
	var data []byte
	if callErr != nil {
		m.Header.Set(hdrCode, strconv.Itoa(int(connect.CodeOf(callErr))))
		data = []byte(callErr.Error())
	} else {
		var err error
		if data, err = proto.Marshal(body); err != nil {
			return err
		}
	}
	if err := c.putPayload(ctx, m, data); err != nil {
		return err
	}
	return c.nc.PublishMsg(m)
}

func (c *Cluster) dispatch(ctx context.Context, method string, data []byte, local pluginhost.Worker, event func(proto.Message)) (proto.Message, error) {
	unmarshal := func(m proto.Message) error {
		if err := proto.Unmarshal(data, m); err != nil {
			return connect.NewError(connect.CodeInvalidArgument, err)
		}
		return nil
	}
	switch method {
	case mDescribe:
		ms, err := local.Describe(ctx)
		return &pluginv1alpha1.DescribeResponse{Evaluators: ms}, err
	case mEvaluate:
		req := &pluginv1alpha1.EvaluateRequest{}
		if err := unmarshal(req); err != nil {
			return nil, err
		}
		return local.Evaluate(ctx, req)
	case mReduce:
		req := &pluginv1alpha1.ReduceRequest{}
		if err := unmarshal(req); err != nil {
			return nil, err
		}
		return local.Reduce(ctx, req)
	case mGenerate:
		req := &pluginv1alpha1.GenerateRequest{}
		if err := unmarshal(req); err != nil {
			return nil, err
		}
		return local.Generate(ctx, req)
	case mLoadDataset:
		req := &pluginv1alpha1.LoadDatasetRequest{}
		if err := unmarshal(req); err != nil {
			return nil, err
		}
		records, err := local.LoadDataset(ctx, req)
		return &pluginv1alpha1.LoadDatasetResponse{Records: records}, err
	case mRunTask:
		req := &pluginv1alpha1.RunTaskRequest{}
		if err := unmarshal(req); err != nil {
			return nil, err
		}
		return local.RunTask(ctx, req, func(ev *harnessv1alpha1.TrajectoryEvent) { event(ev) })
	}
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("unknown method %q", method))
}
