package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/remote"
)

type followupRegistryTransport struct {
	base    http.RoundTripper
	calls   atomic.Int64
	blocked atomic.Bool
}

func (tr *followupRegistryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tr.calls.Add(1)
	if tr.blocked.Load() {
		return nil, errors.New("verification intentionally blocked registry transport")
	}
	return tr.base.RoundTrip(req)
}

func TestAKSFollowupImageCache(t *testing.T) {
	if os.Getenv("EVALSI_AKS_FOLLOWUP_IMAGE_CACHE") != "1" {
		t.Skip("set EVALSI_AKS_FOLLOWUP_IMAGE_CACHE=1 for the owned AKS worker")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	const pinned = "docker.io/library/alpine@sha256:3e9b4b680bfc9fb5269227cffbd6d42be39fbf7c0b908123913864aa4447e764"
	const tag = "docker.io/library/alpine:3.22.6"
	original := remote.DefaultTransport
	tr := &followupRegistryTransport{base: original}
	remote.DefaultTransport = tr
	defer func() { remote.DefaultTransport = original }()

	dir := filepath.Join(t.TempDir(), "images")
	st := newImageStore(dir)
	first, err := st.get(ctx, pinned)
	if err != nil {
		t.Fatal(err)
	}
	firstCalls := tr.calls.Load()
	if firstCalls == 0 {
		t.Fatal("the first real public-image fetch made no observed registry requests")
	}
	if _, err := os.Stat(filepath.Join(first.Root, "etc", "alpine-release")); err != nil {
		t.Fatal(err)
	}

	tr.blocked.Store(true)
	cached, err := st.get(ctx, pinned)
	if err != nil || cached.Root != first.Root || tr.calls.Load() != firstCalls {
		t.Fatalf("warm pinned reuse consulted registry: image=%v err=%v calls=%d", cached, err, tr.calls.Load())
	}
	tr.blocked.Store(false)
	tagged, err := st.get(ctx, tag)
	if err != nil {
		t.Fatal(err)
	}
	tagCalls := tr.calls.Load() - firstCalls
	if tagCalls == 0 || tagged.Digest != first.Digest {
		t.Fatalf("tag did not resolve the expected live digest: image=%v calls=%d", tagged, tagCalls)
	}

	tr.blocked.Store(true)
	before := tr.calls.Load()
	if _, err := st.get(ctx, tag); err == nil || tr.calls.Load() == before {
		t.Fatalf("tag reuse did not attempt a fresh registry lookup: %v", err)
	}
	tagOfflineCalls := tr.calls.Load() - before
	before = tr.calls.Load()
	if _, err := newImageStore(dir).get(ctx, pinned); err == nil || tr.calls.Load() == before {
		t.Fatalf("a fresh store unexpectedly resolved offline from its disk cache: %v", err)
	}
	freshStoreOfflineCalls := tr.calls.Load() - before
	moved := first.Root + "-verification-moved"
	if err := os.Rename(first.Root, moved); err != nil {
		t.Fatal(err)
	}
	before = tr.calls.Load()
	if _, err := st.get(ctx, pinned); err == nil || tr.calls.Load() == before {
		t.Fatalf("missing cached root did not require registry access: %v", err)
	}
	report := map[string]any{
		"pinned": pinned, "tag": tag, "resolved_digest": first.Digest,
		"first_fetch_registry_requests": firstCalls, "warm_pinned_registry_requests": 0,
		"live_tag_resolution_registry_requests": tagCalls,
		"offline_tag_lookup_attempts":           tagOfflineCalls,
		"fresh_store_offline_lookup_attempts":   freshStoreOfflineCalls,
		"missing_root_offline_lookup_attempts":  tr.calls.Load() - before,
		"registry_responses_faked":              false, "mutable_tag_published": false,
		"signature_verifier_enabled": false,
	}
	raw, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("real registry cache controls:\n%s", raw)
	if path := os.Getenv("EVALSI_FOLLOWUP_IMAGE_REPORT"); path != "" {
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
