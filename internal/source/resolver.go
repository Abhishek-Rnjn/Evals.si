package source

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/credentials"
)

// NewResolver reads a source's credential after checking the grants
// (decision 0015). The value is read at each use, so a rotated Secret, and a
// changed grant, take effect without a restart. dir is where Secrets are
// mounted (sources.dir).
func NewResolver(policy *credentials.Policy, dir string) Resolver {
	return func(ctx context.Context, src *evalsiv1alpha1.TraceSource) (string, error) {
		use, ok, err := credentials.SourceUse(src)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", nil
		}
		if err := policy.Check(ctx, src.GetProject(), []credentials.Use{use}); err != nil {
			return "", err
		}
		if env := src.GetCredentials().GetEnv(); env != "" {
			v := strings.TrimSpace(os.Getenv(env))
			if v == "" {
				return "", fmt.Errorf("the source's credential variable %s is not set on the server", env)
			}
			return v, nil
		}
		return readSecretFile(dir, src.GetCredentials().GetFile())
	}
}

// readSecretFile reads "<secret>/<key>" under dir. The name was validated by
// credentials.SourceUse (no dots-only parts, no separators beyond the one).
func readSecretFile(dir, name string) (string, error) {
	if dir == "" {
		return "", fmt.Errorf("source.credentials.file needs sources.dir in the server config (where the chart mounts Secrets)")
	}
	secret, key, ok := strings.Cut(name, "/")
	if !ok || secret == "." || secret == ".." || key == "." || key == ".." || strings.ContainsAny(secret+key, `/\`) {
		return "", fmt.Errorf("source.credentials.file must be <secret>/<key>, not %q", name)
	}
	raw, err := os.ReadFile(filepath.Join(dir, secret, key))
	if err != nil {
		return "", fmt.Errorf("reading the source's credential file: %w", err)
	}
	v := strings.TrimSpace(string(raw))
	if v == "" {
		return "", fmt.Errorf("the source's credential file %s is empty", name)
	}
	return v, nil
}
