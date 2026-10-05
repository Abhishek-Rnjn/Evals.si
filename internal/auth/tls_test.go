package auth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
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
