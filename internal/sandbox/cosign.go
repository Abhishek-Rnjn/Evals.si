package sandbox

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/sigstore/sigstore-go/pkg/root"
)

// SignaturePolicy is sandbox.image_signatures: images must carry a cosign
// signature made with one of the keys, or a keyless one by one of the
// identities, before a sandbox uses them.
//
// Key signatures are read in cosign's registry format (a
// "sha256-<digest>.sig" tag beside the image, cosign 2's default) and
// verified offline against the keys: no transparency log or certificate
// authority is consulted, so the check works air-gapped (copy signatures
// with `cosign copy`).
//
// Keyless signatures (Fulcio certificates, logged in Rekor) are read from the
// same `.sig` tag and from sigstore bundles stored as OCI 1.1 referrers of
// the image, and are verified offline against the trusted root: no call to
// Fulcio or Rekor is made, and the signed entry timestamp or inclusion proof
// in the signature stands for the log (see cosign_keyless.go).
type SignaturePolicy struct {
	// PEM public keys (cosign.pub): files, or the PEM text itself.
	Keys []string `json:"keys,omitempty"`
	// Identities whose keyless signatures are accepted; needs TrustedRoot.
	Keyless []KeylessIdentity `json:"keyless,omitempty"`
	// A sigstore trusted root (trusted_root.json, as `cosign trusted-root
	// create` writes, or the public-good one from the TUF repository): a
	// file, or the JSON itself. It names the Fulcio CAs and Rekor keys to
	// trust, so a private Sigstore deployment works the same way.
	TrustedRoot string `json:"trusted_root,omitempty"`
	// Allow images from local sources (oci-layout:, docker-archive:, dir:),
	// which carry no registry signatures. Default: refused.
	AllowLocal bool `json:"allow_local,omitempty"`
}

func (p *SignaturePolicy) validate() error {
	if len(p.Keys) == 0 && len(p.Keyless) == 0 {
		return errors.New("sandbox.image_signatures needs at least one key or keyless identity")
	}
	if len(p.Keyless) > 0 && p.TrustedRoot == "" {
		return errors.New("sandbox.image_signatures.keyless needs trusted_root (a sigstore trusted_root.json)")
	}
	if _, err := p.publicKeys(); err != nil {
		return err
	}
	_, err := newKeylessVerifier(p, nil)
	return err
}

func (p *SignaturePolicy) publicKeys() ([]crypto.PublicKey, error) {
	var keys []crypto.PublicKey
	for _, k := range p.Keys {
		raw := []byte(k)
		if !strings.Contains(k, "-----BEGIN") {
			var err error
			if raw, err = os.ReadFile(k); err != nil {
				return nil, fmt.Errorf("sandbox.image_signatures: %w", err)
			}
		}
		block, _ := pem.Decode(raw)
		if block == nil {
			return nil, fmt.Errorf("sandbox.image_signatures: %.40q is not a PEM public key", k)
		}
		pub, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("sandbox.image_signatures: %w", err)
		}
		keys = append(keys, pub)
	}
	return keys, nil
}

// signatureVerifier checks images against a policy, remembering digests
// that passed.
type signatureVerifier struct {
	policy *SignaturePolicy
	keys   []crypto.PublicKey
	opts   []remote.Option
	// Set when the policy names keyless identities.
	keyless *keylessVerifier

	mu       sync.Mutex
	verified map[string]bool
}

func newSignatureVerifier(p *SignaturePolicy, opts ...remote.Option) (*signatureVerifier, error) {
	return newSignatureVerifierWithRoot(p, nil, opts...)
}

// newSignatureVerifierWithRoot is newSignatureVerifier with the trusted
// material given (tests stand a virtual Sigstore in for trusted_root).
func newSignatureVerifierWithRoot(p *SignaturePolicy, trusted root.TrustedMaterial, opts ...remote.Option) (*signatureVerifier, error) {
	keys, err := p.publicKeys()
	if err != nil {
		return nil, err
	}
	kv, err := newKeylessVerifier(p, trusted)
	if err != nil {
		return nil, err
	}
	return &signatureVerifier{policy: p, keys: keys, keyless: kv, verified: map[string]bool{}, opts: opts}, nil
}

const (
	cosignSignatureAnnotation = "dev.cosignproject.cosign/signature"
	// Bound what a hostile registry can make us read.
	maxSignatureLayers  = 64
	maxSignaturePayload = 1 << 20
)

// verify checks that ref, which resolved to an image with digest platform,
// is signed. A signature on the reference's own digest (a multi-platform
// index, as `cosign sign` on a tag records) or on the platform image counts.
func (v *signatureVerifier) verify(ctx context.Context, ref, platform string) error {
	if isLocalImage(ref) {
		if v.policy.AllowLocal {
			return nil
		}
		return fmt.Errorf("image %s: image signatures are required and local images carry none (sandbox.image_signatures.allow_local)", ref)
	}
	r, err := name.ParseReference(ref)
	if err != nil {
		return err
	}
	opts := append([]remote.Option{remote.WithContext(ctx), remote.WithAuthFromKeychain(authn.DefaultKeychain)}, v.opts...)
	digests := []string{platform}
	if d, ok := r.(name.Digest); ok {
		digests = append(digests, d.DigestStr())
	} else if desc, err := remote.Head(r, opts...); err == nil {
		digests = append(digests, desc.Digest.String())
	}
	var reasons []string
	for _, d := range digests {
		if v.isVerified(d) {
			return nil
		}
		err := v.verifyDigest(r.Context(), d, opts)
		if err == nil {
			v.mu.Lock()
			v.verified[d] = true
			v.mu.Unlock()
			return nil
		}
		reasons = append(reasons, err.Error())
	}
	return fmt.Errorf("image %s: no valid signature: %s", ref, strings.Join(reasons, "; "))
}

func (v *signatureVerifier) isVerified(digest string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.verified[digest]
}

func isLocalImage(ref string) bool {
	for _, p := range []string{"oci-layout:", "docker-archive:", "dir:"} {
		if strings.HasPrefix(ref, p) {
			return true
		}
	}
	return false
}

// simpleSigning is the payload cosign signs.
type simpleSigning struct {
	Critical struct {
		Image struct {
			Digest string `json:"docker-manifest-digest"`
		} `json:"image"`
		Type string `json:"type"`
	} `json:"critical"`
}

// verifyDigest accepts digest if it has a signature by a trusted key, or a
// keyless one by an allowed identity (in the `.sig` tag or a referrer bundle).
func (v *signatureVerifier) verifyDigest(repo name.Repository, digest string, opts []remote.Option) error {
	err := v.verifySigTag(repo, digest, opts)
	if err == nil || v.keyless == nil {
		return err
	}
	if rerr := v.verifyReferrers(repo, digest, opts); rerr != nil {
		return errors.New(err.Error() + "; " + rerr.Error())
	}
	return nil
}

// verifySigTag reads the `.sig` tag of digest: key signatures, and keyless
// ones when the policy has identities.
func (v *signatureVerifier) verifySigTag(repo name.Repository, digest string, opts []remote.Option) error {
	tag := repo.Tag(strings.Replace(digest, ":", "-", 1) + ".sig")
	img, err := remote.Image(tag, opts...)
	if err != nil {
		var terr *transport.Error
		if errors.As(err, &terr) && terr.StatusCode == 404 {
			return fmt.Errorf("%s is not signed", digest)
		}
		return fmt.Errorf("signatures of %s: %w", digest, err)
	}
	m, err := img.Manifest()
	if err != nil {
		return err
	}
	if len(m.Layers) > maxSignatureLayers {
		return fmt.Errorf("signatures of %s: %d layers", digest, len(m.Layers))
	}
	var keylessErrs []string
	for _, desc := range m.Layers {
		sig, err := base64.StdEncoding.DecodeString(desc.Annotations[cosignSignatureAnnotation])
		if err != nil || len(sig) == 0 || desc.Size > maxSignaturePayload {
			continue
		}
		layer, err := img.LayerByDigest(desc.Digest)
		if err != nil {
			continue
		}
		rc, err := layer.Compressed()
		if err != nil {
			continue
		}
		// Compressed() checks the content against the layer's digest as it is read.
		payload, err := io.ReadAll(io.LimitReader(rc, maxSignaturePayload+1))
		rc.Close()
		if err != nil || len(payload) > maxSignaturePayload {
			continue
		}
		if v.signedByKey(payload, sig) {
			var ss simpleSigning
			if json.Unmarshal(payload, &ss) == nil && ss.Critical.Type == "cosign container image signature" && ss.Critical.Image.Digest == digest {
				return nil
			}
		}
		if v.keyless != nil && desc.Annotations[cosignCertificateAnnotation] != "" {
			err := v.verifyLegacyKeyless(desc.Annotations, payload, sig, digest)
			if err == nil {
				return nil
			}
			keylessErrs = append(keylessErrs, err.Error())
		}
	}
	if len(keylessErrs) > 0 {
		return fmt.Errorf("no signature on %s by a trusted key or allowed identity: %s", digest, strings.Join(keylessErrs, "; "))
	}
	return fmt.Errorf("no signature on %s by a trusted key", digest)
}

func (v *signatureVerifier) signedByKey(payload, sig []byte) bool {
	sum := sha256.Sum256(payload)
	for _, k := range v.keys {
		switch pub := k.(type) {
		case *ecdsa.PublicKey:
			if ecdsa.VerifyASN1(pub, sum[:], sig) {
				return true
			}
		case ed25519.PublicKey:
			if ed25519.Verify(pub, payload, sig) {
				return true
			}
		case *rsa.PublicKey:
			if rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig) == nil ||
				rsa.VerifyPSS(pub, crypto.SHA256, sum[:], sig, nil) == nil {
				return true
			}
		}
	}
	return false
}
