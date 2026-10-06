// Package cluster connects evalsid replicas and workers through NATS
// JetStream, for Kubernetes and other multi-process deployments:
//
//   - Work queues. The control plane sends worker calls (Evaluate, Generate,
//     RunTask, ...) to a stream subject per pool, evalsi.work.<pool>; worker
//     processes (`evalsid worker`) pull them, drive a local Python worker, and
//     answer on the caller's inbox. A worker that dies mid-task leaves the
//     message unacknowledged, and JetStream redelivers it to another one;
//     results are idempotent upserts, so a repeat is harmless. KEDA scales
//     each pool's Deployment on its consumer's backlog.
//   - Spans. Ingest replicas publish received OTLP spans to evalsi.spans; the
//     elected policy engine assembles and evaluates them.
//   - Run events and cancellation across replicas (core NATS subjects).
//
// Payloads over 512 KiB travel through a JetStream object store.
package cluster

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nuid"
)

// Config selects the NATS server.
type Config struct {
	// NATS server URLs, comma-separated (nats://nats:4222). Empty with
	// embedded set: the embedded server.
	URL string `json:"url,omitempty"`
	// A NATS credentials file (JWT and NKey).
	CredsFile string `json:"creds_file,omitempty"`
	// Environment variable holding a token.
	TokenEnv string `json:"token_env,omitempty"`
	// TLS to the NATS server; with cert and key, mutual TLS.
	TLS *TLS `json:"tls,omitempty"`
	// Runs a NATS server with JetStream inside evalsid, for one node (a
	// docker compose setup, or `evalsid worker` processes next to a server).
	Embedded *Embedded `json:"embedded,omitempty"`
	// Stream replicas; 3 on a three-node NATS cluster. Default 1.
	Replicas int `json:"replicas,omitempty"`
	// Cap on the span stream's size in bytes (oldest spans are dropped);
	// default: no cap beyond its 6h age limit.
	SpanMaxBytes int64 `json:"span_max_bytes,omitempty"`
	// How long a worker may hold a task without a heartbeat before it is
	// redelivered; default 60s. Workers heartbeat while they work.
	AckWait string `json:"ack_wait,omitempty"`
}

// TLS configures the client side of TLS to NATS.
type TLS struct {
	CAFile   string `json:"ca_file,omitempty"`
	CertFile string `json:"cert_file,omitempty"`
	KeyFile  string `json:"key_file,omitempty"`
}

// Embedded configures the in-process NATS server.
type Embedded struct {
	// Client listener; default 127.0.0.1:4222.
	Listen string `json:"listen,omitempty"`
	// JetStream storage; default <data_dir>/nats.
	StoreDir string `json:"store_dir,omitempty"`
}

const (
	workStream    = "EVALSI_WORK"
	workPrefix    = "evalsi.work."
	spanStream    = "EVALSI_SPANS"
	spanSubject   = "evalsi.spans"
	payloadBucket = "evalsi-payloads"
	maxInline     = 512 << 10

	hdrMethod  = "Evalsi-Method"
	hdrReply   = "Evalsi-Reply"
	hdrKind    = "Evalsi-Kind"
	hdrCode    = "Evalsi-Code"
	hdrPayload = "Evalsi-Payload"
	hdrProject = "Evalsi-Project"
	hdrLabels  = "Evalsi-Labels"
)

// Pools are the worker pools (DESIGN §14).
var Pools = []string{"cpu", "judge", "gpu", "sandbox", "harness"}

// Cluster is a connection to the NATS server with the streams set up.
type Cluster struct {
	nc      *nats.Conn
	js      jetstream.JetStream
	objects jetstream.ObjectStore
	server  *natsserver.Server
	ackWait time.Duration
	// Identifies this process (leases, logs).
	ID string
}

// Connect connects (starting the embedded server first when configured) and
// creates the streams, consumers and payload store if they do not exist.
func Connect(ctx context.Context, cfg Config, name, dataDir string) (*Cluster, error) {
	c := &Cluster{ID: name + "-" + nuid.Next()[:10], ackWait: 60 * time.Second}
	if cfg.AckWait != "" {
		d, err := time.ParseDuration(cfg.AckWait)
		if err != nil {
			return nil, fmt.Errorf("cluster.ack_wait: %w", err)
		}
		c.ackWait = d
	}
	url := cfg.URL
	if cfg.Embedded != nil {
		srv, err := startEmbedded(*cfg.Embedded, dataDir)
		if err != nil {
			return nil, err
		}
		c.server = srv
		if url == "" {
			url = srv.ClientURL()
		}
	}
	if url == "" {
		return nil, errors.New("cluster: set url, or embedded")
	}
	opts := []nats.Option{nats.Name(name), nats.MaxReconnects(-1), nats.ReconnectWait(time.Second)}
	if cfg.CredsFile != "" {
		opts = append(opts, nats.UserCredentials(cfg.CredsFile))
	}
	if cfg.TokenEnv != "" {
		opts = append(opts, nats.Token(os.Getenv(cfg.TokenEnv)))
	}
	if t := cfg.TLS; t != nil {
		if t.CAFile != "" {
			opts = append(opts, nats.RootCAs(t.CAFile))
		}
		if t.CertFile != "" {
			opts = append(opts, nats.ClientCert(t.CertFile, t.KeyFile))
		}
	}
	nc, err := nats.Connect(url, opts...)
	if err != nil {
		c.shutdownServer()
		return nil, fmt.Errorf("cluster: connecting to %s: %w", url, err)
	}
	c.nc = nc
	if c.js, err = jetstream.New(nc); err != nil {
		c.Close()
		return nil, err
	}
	if err := c.setup(ctx, max(cfg.Replicas, 1), cfg.SpanMaxBytes); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func startEmbedded(cfg Embedded, dataDir string) (*natsserver.Server, error) {
	listen := cfg.Listen
	if listen == "" {
		listen = "127.0.0.1:4222"
	}
	host, portStr, err := net.SplitHostPort(listen)
	if err != nil {
		return nil, fmt.Errorf("cluster.embedded.listen: %w", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, fmt.Errorf("cluster.embedded.listen: %w", err)
	}
	store := cfg.StoreDir
	if store == "" {
		store = dataDir + "/nats"
	}
	srv, err := natsserver.NewServer(&natsserver.Options{
		Host: host, Port: port, JetStream: true, StoreDir: store, NoSigs: true,
		ServerName: "evalsid-embedded",
	})
	if err != nil {
		return nil, fmt.Errorf("cluster: embedded NATS: %w", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(10 * time.Second) {
		srv.Shutdown()
		return nil, errors.New("cluster: embedded NATS did not start")
	}
	return srv, nil
}

func (c *Cluster) setup(ctx context.Context, replicas int, spanMaxBytes int64) error {
	if spanMaxBytes <= 0 {
		spanMaxBytes = -1
	}
	if _, err := c.js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name: workStream, Subjects: []string{workPrefix + "*"},
		Retention: jetstream.WorkQueuePolicy, Storage: jetstream.FileStorage, Replicas: replicas,
		MaxAge: 24 * time.Hour,
	}); err != nil {
		return fmt.Errorf("cluster: work stream: %w", err)
	}
	for _, pool := range Pools {
		if _, err := c.js.CreateOrUpdateConsumer(ctx, workStream, jetstream.ConsumerConfig{
			Durable: "pool-" + pool, FilterSubject: workPrefix + pool,
			AckPolicy: jetstream.AckExplicitPolicy, AckWait: c.ackWait, MaxDeliver: 5,
			MaxAckPending: 10000,
		}); err != nil {
			return fmt.Errorf("cluster: %s pool consumer: %w", pool, err)
		}
	}
	if _, err := c.js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name: spanStream, Subjects: []string{spanSubject},
		Retention: jetstream.LimitsPolicy, Storage: jetstream.FileStorage, Replicas: replicas,
		MaxAge: 6 * time.Hour, MaxBytes: spanMaxBytes, Discard: jetstream.DiscardOld,
	}); err != nil {
		return fmt.Errorf("cluster: span stream: %w", err)
	}
	if _, err := c.js.CreateOrUpdateConsumer(ctx, spanStream, jetstream.ConsumerConfig{
		Durable: "policy-engine", AckPolicy: jetstream.AckExplicitPolicy, AckWait: c.ackWait,
		MaxAckPending: 100000,
	}); err != nil {
		return fmt.Errorf("cluster: span consumer: %w", err)
	}
	obj, err := c.js.CreateOrUpdateObjectStore(ctx, jetstream.ObjectStoreConfig{
		Bucket: payloadBucket, TTL: 6 * time.Hour, Storage: jetstream.FileStorage, Replicas: replicas,
	})
	if err != nil {
		return fmt.Errorf("cluster: payload store: %w", err)
	}
	c.objects = obj
	return nil
}

// Owner identifies this process in leases.
func (c *Cluster) Owner() string { return c.ID }

// Close drains the connection and stops the embedded server.
func (c *Cluster) Close() {
	if c.nc != nil {
		_ = c.nc.Drain()
		for i := 0; i < 50 && !c.nc.IsClosed(); i++ {
			time.Sleep(20 * time.Millisecond)
		}
		c.nc.Close()
	}
	c.shutdownServer()
}

func (c *Cluster) shutdownServer() {
	if c.server != nil {
		c.server.Shutdown()
		c.server.WaitForShutdown()
	}
}

// ClientURL is the embedded server's address ("" without one).
func (c *Cluster) ClientURL() string {
	if c.server == nil {
		return ""
	}
	return c.server.ClientURL()
}

// Backlog reports a pool's pending messages (what KEDA scales on).
func (c *Cluster) Backlog(ctx context.Context, pool string) (uint64, error) {
	cons, err := c.js.Consumer(ctx, workStream, "pool-"+pool)
	if err != nil {
		return 0, err
	}
	info, err := cons.Info(ctx)
	if err != nil {
		return 0, err
	}
	return info.NumPending + uint64(info.NumAckPending), nil
}

// putPayload stores data that is too large for a message and returns the
// header that names it; small data stays in the message.
func (c *Cluster) putPayload(ctx context.Context, msg *nats.Msg, data []byte) error {
	if len(data) <= maxInline {
		msg.Data = data
		return nil
	}
	name := nuid.Next()
	if _, err := c.objects.PutBytes(ctx, name, data); err != nil {
		return fmt.Errorf("cluster: storing a %d-byte payload: %w", len(data), err)
	}
	msg.Header.Set(hdrPayload, name)
	return nil
}

// payload returns a message's data, from the payload store when it is there.
func (c *Cluster) payload(ctx context.Context, h nats.Header, data []byte) ([]byte, error) {
	name := h.Get(hdrPayload)
	if name == "" {
		return data, nil
	}
	return c.objects.GetBytes(ctx, name)
}

func (c *Cluster) dropPayload(h nats.Header) {
	if name := h.Get(hdrPayload); name != "" {
		_ = c.objects.Delete(context.Background(), name)
	}
}
