// Package controllers holds the operator's reconcilers. EvalRun and
// OnlineEvalPolicy resources are synced to evalsid through its API (the
// operator is one more API client, so runs it creates are audited and
// authorized like any other); Evaluator and SandboxClass resources become
// worker and sandbox-pool Deployments.
package controllers

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"

	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1/evalsiv1alpha1connect"
)

// API reaches evalsid.
type API struct {
	Runs    evalsiv1alpha1connect.RunServiceClient
	Monitor evalsiv1alpha1connect.MonitorServiceClient
}

// APIConfig says how to reach evalsid.
type APIConfig struct {
	// The server's base URL, e.g. https://evalsi.evalsi.svc:8080.
	URL string
	// A bearer token, read on every call so a projected service-account
	// token or a rotated key is picked up; or a fixed token.
	TokenFile string
	Token     string
	// CA for an https URL; client certificate for mutual TLS.
	CAFile, CertFile, KeyFile string
}

func NewAPI(cfg APIConfig) (*API, error) {
	if cfg.URL == "" {
		return nil, errors.New("the evalsid URL is required")
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if strings.HasPrefix(cfg.URL, "https://") {
		tc := &tls.Config{MinVersion: tls.VersionTLS12}
		if cfg.CAFile != "" {
			pem, err := os.ReadFile(cfg.CAFile)
			if err != nil {
				return nil, err
			}
			tc.RootCAs = x509.NewCertPool()
			if !tc.RootCAs.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("%s: no certificates", cfg.CAFile)
			}
		}
		if cfg.CertFile != "" {
			cert, key := cfg.CertFile, cfg.KeyFile
			// Reloaded per handshake: cert-manager rotates them in place.
			tc.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
				c, err := tls.LoadX509KeyPair(cert, key)
				return &c, err
			}
		}
		tr.TLSClientConfig = tc
	}
	client := &http.Client{Transport: tr, Timeout: time.Minute}
	auth := connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			token := cfg.Token
			if cfg.TokenFile != "" {
				b, err := os.ReadFile(cfg.TokenFile)
				if err != nil {
					return nil, err
				}
				token = strings.TrimSpace(string(b))
			}
			if token != "" {
				req.Header().Set("Authorization", "Bearer "+token)
			}
			return next(ctx, req)
		}
	})
	base := strings.TrimSuffix(cfg.URL, "/")
	opts := connect.WithInterceptors(auth)
	return &API{
		Runs:    evalsiv1alpha1connect.NewRunServiceClient(client, base, opts),
		Monitor: evalsiv1alpha1connect.NewMonitorServiceClient(client, base, opts),
	}, nil
}

// permanent reports API errors that retrying the same request cannot fix.
func permanent(err error) bool {
	switch connect.CodeOf(err) {
	case connect.CodeInvalidArgument, connect.CodePermissionDenied, connect.CodeFailedPrecondition,
		connect.CodeAlreadyExists, connect.CodeOutOfRange, connect.CodeUnimplemented:
		return true
	}
	return false
}

func hash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}
