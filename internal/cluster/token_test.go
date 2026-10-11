package cluster

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
)

// The chart's NATS requires a token (--auth): clients send it from
// cluster.token_env, and a client without it is refused.
func TestTokenAuth(t *testing.T) {
	host, port, _ := net.SplitHostPort(freePort(t))
	p, _ := strconv.Atoi(port)
	srv, err := natsserver.NewServer(&natsserver.Options{
		Host: host, Port: p, JetStream: true, StoreDir: t.TempDir(), NoSigs: true, Authorization: "s3cret",
	})
	if err != nil {
		t.Fatal(err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(10 * time.Second) {
		t.Fatal("NATS did not start")
	}
	t.Cleanup(srv.Shutdown)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	url := "nats://" + net.JoinHostPort(host, port)
	if c, err := Connect(ctx, Config{URL: url}, "anonymous", t.TempDir()); err == nil {
		c.Close()
		t.Fatal("connected without the token")
	}
	t.Setenv("EVALSI_NATS_TOKEN", "s3cret")
	c, err := Connect(ctx, Config{URL: url, TokenEnv: "EVALSI_NATS_TOKEN"}, "evalsid", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
}
