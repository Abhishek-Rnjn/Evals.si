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
)

// SignaturePolicy is sandbox.image_signatures: images must carry a cosign
// signature made with one of the keys before a sandbox uses them.
//
// Signatures are read in cosign's registry format (a "sha256-<digest>.sig"
// tag beside the image, cosign 2's default) and verified offline against
// the keys: no transparency log or certificate authority is consulted, so
// the check works air-gapped (copy signatures with `cosign copy`).
type SignaturePolicy struct {
	// PEM public keys (cosign.pub): files, or the PEM text itself.
	Keys []string `json:"keys"`
	// Allow images from local sources (oci-layout:, docker-archive:, dir:),
	// which carry no registry signatures. Default: refused.
	AllowLocal bool `json:"allow_local,omitempty"`
}

func (p *SignaturePolicy) validate() error {
	if len(p.Keys) == 0 {
		return errors.New("sandbox.image_signatures needs at least one key")
	}
	_, err := p.publicKeys()
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

	mu       sync.Mutex
	verified map[string]bool
}

func newSignatureVerifier(p *SignaturePolicy, opts ...remote.Option) (*signatureVerifier, error) {
	keys, err := p.publicKeys()
	if err != nil {
		return nil, err
	}
	return &signatureVerifier{policy: p, keys: keys, verified: map[string]bool{}, opts: opts}, nil
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

func (v *signatureVerifier) verifyDigest(repo name.Repository, digest string, opts []remote.Option) error {
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
		if !v.signedByKey(payload, sig) {
			continue
		}
		var ss simpleSigning
		if json.Unmarshal(payload, &ss) != nil || ss.Critical.Type != "cosign container image signature" {
			continue
		}
		if ss.Critical.Image.Digest == digest {
			return nil
		}
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
