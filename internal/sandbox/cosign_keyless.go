package sandbox

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	protobundle "github.com/sigstore/protobuf-specs/gen/pb-go/bundle/v1"
	protocommon "github.com/sigstore/protobuf-specs/gen/pb-go/common/v1"
	protorekor "github.com/sigstore/protobuf-specs/gen/pb-go/rekor/v1"
	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

// KeylessIdentity is who may have signed an image keylessly: the Fulcio
// certificate's OIDC issuer and subject (its SAN), each exact or a regular
// expression, as in `cosign verify --certificate-identity[-regexp]` and
// `--certificate-oidc-issuer[-regexp]`. Issuer and subject each need one of
// the pair.
type KeylessIdentity struct {
	Issuer        string `json:"issuer,omitempty"`
	IssuerRegexp  string `json:"issuer_regexp,omitempty"`
	Subject       string `json:"subject,omitempty"`
	SubjectRegexp string `json:"subject_regexp,omitempty"`
}

func (k KeylessIdentity) certificateIdentity() (verify.CertificateIdentity, error) {
	if (k.Issuer == "") == (k.IssuerRegexp == "") || (k.Subject == "") == (k.SubjectRegexp == "") {
		return verify.CertificateIdentity{}, errors.New("sandbox.image_signatures.keyless: each identity needs exactly one of issuer or issuer_regexp, and one of subject or subject_regexp")
	}
	return verify.NewShortCertificateIdentity(k.Issuer, k.IssuerRegexp, k.Subject, k.SubjectRegexp)
}

// Annotations of a keyless signature in cosign's `.sig` tag format.
const (
	cosignCertificateAnnotation = "dev.sigstore.cosign/certificate"
	cosignBundleAnnotation      = "dev.sigstore.cosign/bundle"
	// Sigstore bundles stored as OCI 1.1 referrers have an artifactType with
	// this prefix, and the bundle JSON as the manifest's one layer.
	sigstoreBundleArtifactPrefix = "application/vnd.dev.sigstore.bundle"
	maxBundleBytes               = 4 << 20
	maxReferrerBundles           = 16
)

// keylessVerifier verifies Fulcio-certificate signatures against a trusted
// root (Fulcio CAs, Rekor keys, optionally timestamp authorities): the
// certificate chains to the root and was valid when Rekor logged the
// signature, the log entry carries a valid signed entry timestamp or
// inclusion proof (checked offline, against the root's Rekor keys), the
// signature matches, and the certificate names one of the allowed
// identities.
type keylessVerifier struct {
	verifier   *verify.Verifier
	identities []verify.CertificateIdentity
}

func newKeylessVerifier(p *SignaturePolicy, trusted root.TrustedMaterial) (*keylessVerifier, error) {
	if len(p.Keyless) == 0 {
		return nil, nil
	}
	if trusted == nil {
		raw := []byte(p.TrustedRoot)
		if !strings.HasPrefix(strings.TrimSpace(p.TrustedRoot), "{") {
			var err error
			if raw, err = os.ReadFile(p.TrustedRoot); err != nil {
				return nil, fmt.Errorf("sandbox.image_signatures.trusted_root: %w", err)
			}
		}
		tr, err := root.NewTrustedRootFromJSON(raw)
		if err != nil {
			return nil, fmt.Errorf("sandbox.image_signatures.trusted_root: %w", err)
		}
		trusted = tr
	}
	// One Rekor entry the trusted root's log keys vouch for (a signed entry
	// timestamp, or an inclusion proof with a signed checkpoint), and one
	// observer timestamp (the entry's signed timestamp or an RFC 3161 one from
	// a trusted authority) for the time the short-lived certificate is checked at.
	v, err := verify.NewVerifier(trusted, verify.WithTransparencyLog(1), verify.WithObserverTimestamps(1))
	if err != nil {
		return nil, err
	}
	kv := &keylessVerifier{verifier: v}
	for _, k := range p.Keyless {
		id, err := k.certificateIdentity()
		if err != nil {
			return nil, err
		}
		kv.identities = append(kv.identities, id)
	}
	return kv, nil
}

// verifyEntity checks a bundle against the policy's identities, for the
// artifact the option names (made afresh per identity: a reader is consumed).
func (k *keylessVerifier) verifyEntity(entity verify.SignedEntity, artifact func() verify.ArtifactPolicyOption) error {
	var errs []error
	for _, id := range k.identities {
		_, err := k.verifier.Verify(entity, verify.NewPolicy(artifact(), verify.WithCertificateIdentity(id)))
		if err == nil {
			return nil
		}
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// verifyReferrers looks for sigstore bundles that refer to the image digest
// (OCI 1.1 referrers API, or the `sha256-<digest>` tag fallback that
// go-containerregistry applies) and accepts the image if one verifies for
// that digest.
func (v *signatureVerifier) verifyReferrers(repo name.Repository, digest string, opts []remote.Option) error {
	subject, err := name.NewDigest(repo.Name() + "@" + digest)
	if err != nil {
		return err
	}
	index, err := remote.Referrers(subject, opts...)
	if err != nil {
		return fmt.Errorf("referrers of %s: %w", digest, err)
	}
	manifest, err := index.IndexManifest()
	if err != nil {
		return err
	}
	raw, err := hex.DecodeString(strings.TrimPrefix(digest, "sha256:"))
	if err != nil || !strings.HasPrefix(digest, "sha256:") {
		return fmt.Errorf("unsupported digest %q", digest)
	}
	var reasons []string
	seen := 0
	for _, desc := range manifest.Manifests {
		if !strings.HasPrefix(desc.ArtifactType, sigstoreBundleArtifactPrefix) {
			continue
		}
		if seen++; seen > maxReferrerBundles {
			break
		}
		b, err := fetchReferrerBundle(repo, desc, opts)
		if err == nil {
			err = v.keyless.verifyEntity(b, func() verify.ArtifactPolicyOption { return verify.WithArtifactDigest("sha256", raw) })
		}
		if err == nil {
			return nil
		}
		reasons = append(reasons, err.Error())
	}
	if seen == 0 {
		return fmt.Errorf("no sigstore bundle refers to %s", digest)
	}
	return fmt.Errorf("no sigstore bundle on %s is by an allowed identity: %s", digest, strings.Join(reasons, "; "))
}

func fetchReferrerBundle(repo name.Repository, desc v1.Descriptor, opts []remote.Option) (*bundle.Bundle, error) {
	img, err := remote.Image(repo.Digest(desc.Digest.String()), opts...)
	if err != nil {
		return nil, err
	}
	layers, err := img.Layers()
	if err != nil || len(layers) != 1 {
		return nil, fmt.Errorf("bundle %s: want one layer", desc.Digest)
	}
	rc, err := layers[0].Compressed()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	// Compressed() checks the content against the layer's digest as it is read.
	data, err := io.ReadAll(io.LimitReader(rc, maxBundleBytes+1))
	if err != nil || len(data) > maxBundleBytes {
		return nil, fmt.Errorf("bundle %s: unreadable or too large", desc.Digest)
	}
	var b bundle.Bundle
	if err := b.UnmarshalJSON(data); err != nil {
		return nil, fmt.Errorf("bundle %s: %w", desc.Digest, err)
	}
	return &b, nil
}

// rekorLegacyBundle is the `dev.sigstore.cosign/bundle` annotation.
type rekorLegacyBundle struct {
	SignedEntryTimestamp []byte `json:"SignedEntryTimestamp"`
	Payload              struct {
		Body           string `json:"body"`
		IntegratedTime int64  `json:"integratedTime"`
		LogIndex       int64  `json:"logIndex"`
		LogID          string `json:"logID"`
	} `json:"Payload"`
}

// legacyKeylessBundle turns one cosign `.sig` layer that carries a Fulcio
// certificate and a Rekor bundle (signed entry timestamp) into a sigstore
// bundle over the layer's payload, so one verifier handles both formats.
func legacyKeylessBundle(annotations map[string]string, payload, sig []byte) (*bundle.Bundle, error) {
	block, _ := pem.Decode([]byte(annotations[cosignCertificateAnnotation]))
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("no signing certificate")
	}
	var rb rekorLegacyBundle
	if err := json.Unmarshal([]byte(annotations[cosignBundleAnnotation]), &rb); err != nil || len(rb.SignedEntryTimestamp) == 0 {
		return nil, errors.New("no Rekor bundle")
	}
	body, err := base64.StdEncoding.DecodeString(rb.Payload.Body)
	if err != nil {
		return nil, fmt.Errorf("rekor bundle body: %w", err)
	}
	logID, err := hex.DecodeString(rb.Payload.LogID)
	if err != nil {
		return nil, fmt.Errorf("rekor bundle log ID: %w", err)
	}
	var entry struct {
		Kind       string `json:"kind"`
		APIVersion string `json:"apiVersion"`
	}
	if err := json.Unmarshal(body, &entry); err != nil || entry.Kind == "" {
		return nil, errors.New("rekor entry body is not a Rekor v1 entry")
	}
	sum := sha256.Sum256(payload)
	return bundle.NewBundle(&protobundle.Bundle{
		MediaType: "application/vnd.dev.sigstore.bundle+json;version=0.1",
		VerificationMaterial: &protobundle.VerificationMaterial{
			Content: &protobundle.VerificationMaterial_X509CertificateChain{
				X509CertificateChain: &protocommon.X509CertificateChain{Certificates: []*protocommon.X509Certificate{{RawBytes: block.Bytes}}},
			},
			TlogEntries: []*protorekor.TransparencyLogEntry{{
				LogIndex:          rb.Payload.LogIndex,
				LogId:             &protocommon.LogId{KeyId: logID},
				KindVersion:       &protorekor.KindVersion{Kind: entry.Kind, Version: entry.APIVersion},
				IntegratedTime:    rb.Payload.IntegratedTime,
				InclusionPromise:  &protorekor.InclusionPromise{SignedEntryTimestamp: rb.SignedEntryTimestamp},
				CanonicalizedBody: body,
			}},
		},
		Content: &protobundle.Bundle_MessageSignature{MessageSignature: &protocommon.MessageSignature{
			MessageDigest: &protocommon.HashOutput{Algorithm: protocommon.HashAlgorithm_SHA2_256, Digest: sum[:]},
			Signature:     sig,
		}},
	})
}

// verifyLegacyKeyless checks one `.sig` layer for a keyless signature: the
// certificate and Rekor entry verify (see keylessVerifier) for the payload,
// and the payload names this digest.
func (v *signatureVerifier) verifyLegacyKeyless(annotations map[string]string, payload, sig []byte, digest string) error {
	var ss simpleSigning
	if json.Unmarshal(payload, &ss) != nil || ss.Critical.Type != "cosign container image signature" || ss.Critical.Image.Digest != digest {
		return errors.New("payload does not sign this digest")
	}
	b, err := legacyKeylessBundle(annotations, payload, sig)
	if err != nil {
		return err
	}
	return v.keyless.verifyEntity(b, func() verify.ArtifactPolicyOption { return verify.WithArtifact(bytes.NewReader(payload)) })
}
