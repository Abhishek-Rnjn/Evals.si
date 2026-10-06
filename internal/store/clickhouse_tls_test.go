package store

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/abhishek-rnjn/evals.si/internal/auth"
)

func selfSigned(t *testing.T, cn string) (cert, key string) {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	kder, _ := x509.MarshalECPrivateKey(k)
	dir := t.TempDir()
	cert, key = filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	_ = os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	_ = os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600)
	return cert, key
}

// ClickHouse over mutual TLS with a private CA: the schema statements reach
// a server that requires the client's certificate.
func TestClickHouseMutualTLS(t *testing.T) {
	serverCert, serverKey := selfSigned(t, "clickhouse")
	clientCert, clientKey := selfSigned(t, "evalsid")
	tc, err := auth.ServerTLS(&auth.TLSConfig{CertFile: serverCert, KeyFile: serverKey, ClientCA: clientCert, RequireClientCert: true})
	if err != nil {
		t.Fatal(err)
	}
	// Without SNI (an IP address) httptest would serve its own certificate.
	pair, _ := tls.LoadX509KeyPair(serverCert, serverKey)
	tc.Certificates = []tls.Certificate{pair}
	var statements atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS.PeerCertificates[0].Subject.CommonName == "evalsid" {
			statements.Add(1)
		}
	}))
	srv.TLS = tc
	srv.StartTLS()
	defer srv.Close()

	ctx := context.Background()
	ts, err := OpenClickHouse(ctx, ClickHouseConfig{URL: srv.URL, TLS: &auth.ClientTLSConfig{CAFile: serverCert, CertFile: clientCert, KeyFile: clientKey}}, "")
	if err != nil {
		t.Fatal(err)
	}
	_ = ts
	if statements.Load() == 0 {
		t.Error("no statements arrived")
	}
	if _, err := OpenClickHouse(ctx, ClickHouseConfig{URL: srv.URL, TLS: &auth.ClientTLSConfig{CAFile: serverCert}}, ""); err == nil {
		t.Error("opened without a client certificate")
	}
}
