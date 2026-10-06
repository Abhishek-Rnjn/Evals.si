package auth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writePair(t *testing.T, dir, cn string, at time.Time) (string, string) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"localhost"}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, _ := x509.MarshalECPrivateKey(key)
	cert, keyFile := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{cert, keyFile} {
		_ = os.Chtimes(f, at, at)
	}
	return cert, keyFile
}

func TestServerTLSReloads(t *testing.T) {
	dir := t.TempDir()
	cert, key := writePair(t, dir, "first", time.Now().Add(-time.Minute))
	if _, err := ServerTLS(&TLSConfig{CertFile: cert, KeyFile: key}); err != nil {
		t.Fatal(err)
	}
	if _, err := ServerTLS(&TLSConfig{CertFile: cert, KeyFile: filepath.Join(dir, "missing")}); err == nil {
		t.Error("missing key accepted")
	}
	r := &certReloader{certFile: cert, keyFile: key}
	if err := r.load(); err != nil {
		t.Fatal(err)
	}
	leaf := func() string {
		c, err := r.get(nil)
		if err != nil {
			t.Fatal(err)
		}
		parsed, _ := x509.ParseCertificate(c.Certificate[0])
		return parsed.Subject.CommonName
	}
	if leaf() != "first" {
		t.Fatal("initial certificate not served")
	}
	writePair(t, dir, "second", time.Now())
	r.checked = time.Time{} // the reloader looks at most every few seconds
	if got := leaf(); got != "second" {
		t.Errorf("after rotation serving %q", got)
	}
	// A half-written pair keeps the old certificate in service.
	if err := os.WriteFile(key, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(key, time.Now().Add(time.Minute), time.Now().Add(time.Minute))
	r.checked = time.Time{}
	if got := leaf(); got != "second" {
		t.Errorf("broken pair replaced the certificate: %q", got)
	}
}

// ClientTLS trusts a private CA and presents a client certificate that a
// server requiring one accepts.
func TestClientTLS(t *testing.T) {
	serverCert, serverKey := writePair(t, t.TempDir(), "localhost", time.Now())
	clientCert, clientKey := writePair(t, t.TempDir(), "evalsid", time.Now())
	srvCfg, err := ServerTLS(&TLSConfig{CertFile: serverCert, KeyFile: serverKey, ClientCA: clientCert, RequireClientCert: true})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.TLS.PeerCertificates[0].Subject.CommonName))
	}))
	srv.TLS = srvCfg
	srv.StartTLS()
	defer srv.Close()
	url := strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)

	get := func(c *ClientTLSConfig) (string, error) {
		tc, err := ClientTLS(c)
		if err != nil {
			return "", err
		}
		resp, err := (&http.Client{Transport: &http.Transport{TLSClientConfig: tc}}).Get(url)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b), nil
	}
	if got, err := get(&ClientTLSConfig{CAFile: serverCert, CertFile: clientCert, KeyFile: clientKey}); err != nil || got != "evalsid" {
		t.Fatalf("mutual TLS: %q %v", got, err)
	}
	if _, err := get(&ClientTLSConfig{CAFile: serverCert}); err == nil {
		t.Error("connected without a client certificate")
	}
	if _, err := get(&ClientTLSConfig{CertFile: clientCert, KeyFile: clientKey}); err == nil {
		t.Error("trusted a private CA without ca_file")
	}
	if _, err := ClientTLS(&ClientTLSConfig{CertFile: clientCert}); err == nil {
		t.Error("a certificate without its key")
	}
}
