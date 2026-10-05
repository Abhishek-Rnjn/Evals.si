package auth

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// ServerTLS builds the API listener's TLS config. The certificate and key are
// re-read when either file changes, checked at most every few seconds, so
// rotation (cert-manager, ACME) needs no restart.
func ServerTLS(c *TLSConfig) (*tls.Config, error) {
	r := &certReloader{certFile: c.CertFile, keyFile: c.KeyFile}
	if err := r.load(); err != nil {
		return nil, err
	}
	cfg := &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: r.get,
		NextProtos:     []string{"h2", "http/1.1"},
	}
	if c.ClientCA != "" {
		pem, err := os.ReadFile(c.ClientCA)
		if err != nil {
			return nil, fmt.Errorf("auth.tls.client_ca: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("auth.tls.client_ca: no certificates found")
		}
		cfg.ClientCAs = pool
		cfg.ClientAuth = tls.VerifyClientCertIfGiven
		if c.RequireClientCert {
			cfg.ClientAuth = tls.RequireAndVerifyClientCert
		}
	}
	return cfg, nil
}

type certReloader struct {
	certFile, keyFile string

	mu      sync.Mutex
	cert    *tls.Certificate
	modTime time.Time
	checked time.Time
}

func (r *certReloader) stamp() (time.Time, error) {
	var latest time.Time
	for _, f := range []string{r.certFile, r.keyFile} {
		info, err := os.Stat(f)
		if err != nil {
			return time.Time{}, err
		}
		if info.ModTime().After(latest) {
			latest = info.ModTime()
		}
	}
	return latest, nil
}

func (r *certReloader) load() error {
	stamp, err := r.stamp()
	if err != nil {
		return fmt.Errorf("auth.tls: %w", err)
	}
	cert, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		return fmt.Errorf("auth.tls: %w", err)
	}
	r.cert, r.modTime = &cert, stamp
	return nil
}

func (r *certReloader) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if now := time.Now(); now.Sub(r.checked) > 5*time.Second {
		r.checked = now
		if stamp, err := r.stamp(); err == nil && stamp.After(r.modTime) {
			// A half-written pair fails to load; keep serving the old one until both are in place.
			_ = r.load()
		}
	}
	return r.cert, nil
}
