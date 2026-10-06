package sandbox

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

func testRegistry(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func pushRandom(t *testing.T, ref string) v1.Hash {
	t.Helper()
	img, err := random.Image(256, 1)
	if err != nil {
		t.Fatal(err)
	}
	r, err := name.ParseReference(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(r, img); err != nil {
		t.Fatal(err)
	}
	d, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func newKey(t *testing.T) (*ecdsa.PrivateKey, string) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return k, string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

// sign stores a cosign-format signature of digest (claiming claimed) under
// repo's sha256-<digest>.sig tag.
func sign(t *testing.T, repo string, digest, claimed v1.Hash, key *ecdsa.PrivateKey) {
	t.Helper()
	payload := fmt.Appendf(nil, `{"critical":{"identity":{"docker-reference":%q},"image":{"docker-manifest-digest":%q},"type":"cosign container image signature"},"optional":null}`, repo, claimed)
	sum := sha256.Sum256(payload)
	sig, err := ecdsa.SignASN1(rand.Reader, key, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	layer := static.NewLayer(payload, types.MediaType("application/vnd.dev.cosign.simplesigning.v1+json"))
	img, err := mutate.Append(empty.Image, mutate.Addendum{
		Layer:       layer,
		Annotations: map[string]string{cosignSignatureAnnotation: base64.StdEncoding.EncodeToString(sig)},
	})
	if err != nil {
		t.Fatal(err)
	}
	tag, err := name.NewTag(repo + ":" + strings.Replace(digest.String(), ":", "-", 1) + ".sig")
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(tag, img); err != nil {
		t.Fatal(err)
	}
}

func TestImageSignatures(t *testing.T) {
	ctx := context.Background()
	reg := testRegistry(t)
	repo := reg + "/tasks/env"
	trusted, trustedPEM := newKey(t)
	other, _ := newKey(t)

	signed := pushRandom(t, repo+":signed")
	sign(t, repo, signed, signed, trusted)
	byOther := pushRandom(t, repo+":other")
	sign(t, repo, byOther, byOther, other)
	unsigned := pushRandom(t, repo+":unsigned")
	// A valid signature of another image, copied under this image's tag.
	replayed := pushRandom(t, repo+":replayed")
	sign(t, repo, replayed, signed, trusted)

	v, err := newSignatureVerifier(&SignaturePolicy{Keys: []string{trustedPEM}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		ref, platform, wantErr string
	}{
		{repo + "@" + signed.String(), signed.String(), ""},
		// Signed on the tag's digest (an index, say); the platform image is not.
		{repo + ":signed", "sha256:" + strings.Repeat("0", 64), ""},
		{repo + ":other", byOther.String(), "by a trusted key"},
		{repo + ":unsigned", unsigned.String(), "is not signed"},
		{repo + ":replayed", replayed.String(), "by a trusted key"},
		{"oci-layout:/somewhere", "", "local images carry none"},
	} {
		err := v.verify(ctx, tc.ref, tc.platform)
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%s: %v", tc.ref, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%s: got %v, want %q", tc.ref, err, tc.wantErr)
		}
	}

	local, _ := newSignatureVerifier(&SignaturePolicy{Keys: []string{trustedPEM}, AllowLocal: true})
	if err := local.verify(ctx, "dir:/x", ""); err != nil {
		t.Errorf("allow_local: %v", err)
	}

	// Wired into the image store: an unsigned image is never unpacked.
	st := newImageStore(t.TempDir())
	st.verifier = v
	if _, err := st.get(ctx, repo+":unsigned"); err == nil || !strings.Contains(err.Error(), "no valid signature") {
		t.Errorf("unsigned image: %v", err)
	}
	if entries, _ := os.ReadDir(st.dir); len(entries) > 0 {
		t.Errorf("unsigned image was unpacked: %v", entries)
	}
	if _, err := st.get(ctx, repo+":signed"); err != nil {
		t.Errorf("signed image: %v", err)
	}
}

func TestSignaturePolicyConfig(t *testing.T) {
	for _, p := range []SignaturePolicy{{}, {Keys: []string{"/no/such/key.pub"}}, {Keys: []string{"-----BEGIN PUBLIC KEY-----\nnope\n-----END PUBLIC KEY-----\n"}}} {
		if err := (Config{ImageSignatures: &p}).Validate(); err == nil {
			t.Errorf("%+v was accepted", p)
		}
	}
}

// Signatures made by the cosign CLI verify (when cosign is installed).
func TestImageSignaturesFromCosign(t *testing.T) {
	bin, err := exec.LookPath("cosign")
	if err != nil {
		t.Skip("cosign not installed")
	}
	reg := testRegistry(t)
	repo := reg + "/tasks/env"
	digest := pushRandom(t, repo+":v1")
	dir := t.TempDir()
	env := append(os.Environ(), "COSIGN_PASSWORD=", "COSIGN_YES=true")
	run := func(args ...string) {
		cmd := exec.Command(bin, args...)
		cmd.Dir, cmd.Env = dir, env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("cosign %v: %v\n%s", args, err, out)
		}
	}
	run("generate-key-pair")
	run("sign", "--key", "cosign.key", "--tlog-upload=false", "--allow-http-registry", repo+"@"+digest.String())

	v, err := newSignatureVerifier(&SignaturePolicy{Keys: []string{filepath.Join(dir, "cosign.pub")}})
	if err != nil {
		t.Fatal(err)
	}
	if err := v.verify(context.Background(), repo+":v1", digest.String()); err != nil {
		t.Fatal(err)
	}
	_, otherPEM := newKey(t)
	v2, _ := newSignatureVerifier(&SignaturePolicy{Keys: []string{otherPEM}})
	if err := v2.verify(context.Background(), repo+":v1", digest.String()); err == nil {
		t.Fatal("verified with the wrong key")
	}
}
