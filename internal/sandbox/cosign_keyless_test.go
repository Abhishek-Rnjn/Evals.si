package sandbox

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
	protobundle "github.com/sigstore/protobuf-specs/gen/pb-go/bundle/v1"
	protocommon "github.com/sigstore/protobuf-specs/gen/pb-go/common/v1"
	protodsse "github.com/sigstore/protobuf-specs/gen/pb-go/dsse"
	protorekor "github.com/sigstore/protobuf-specs/gen/pb-go/rekor/v1"
	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/testing/ca"
	"github.com/sigstore/sigstore-go/pkg/tlog"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// These tests use sigstore-go's virtual Sigstore: a test CA standing in for
// Fulcio, a test Rekor key (signed entry timestamps and, for one test, an
// inclusion proof with a signed checkpoint) and a test timestamp authority.
// Nothing here is checked against the public-good Fulcio or Rekor, or
// against signatures made by the cosign CLI.

const (
	testIssuer  = "https://token.actions.example.com"
	testSubject = "https://github.com/acme/images/.github/workflows/release.yml@refs/tags/v1"
)

func keylessPolicy(ids ...KeylessIdentity) *SignaturePolicy {
	return &SignaturePolicy{Keyless: ids, TrustedRoot: "unused: the test supplies the trusted material"}
}

func rekorSET(t *testing.T, vs *ca.VirtualSigstore, tle *protorekor.TransparencyLogEntry) []byte {
	t.Helper()
	logID, err := vs.RekorLogID()
	if err != nil {
		t.Fatal(err)
	}
	set, err := vs.RekorSignPayload(tlog.RekorPayload{
		Body: base64.StdEncoding.EncodeToString(tle.CanonicalizedBody), IntegratedTime: tle.IntegratedTime,
		LogIndex: tle.LogIndex, LogID: logID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return set
}

// signLegacyKeyless stores a cosign-format keyless signature (certificate,
// signature and Rekor bundle as layer annotations) of digest, claiming claimed.
func signLegacyKeyless(t *testing.T, vs *ca.VirtualSigstore, repo string, digest, claimed v1.Hash, subject, issuer string) {
	t.Helper()
	payload := fmt.Appendf(nil, `{"critical":{"identity":{"docker-reference":%q},"image":{"docker-manifest-digest":%q},"type":"cosign container image signature"},"optional":null}`, repo, claimed)
	entity, err := vs.Sign(subject, issuer, payload)
	if err != nil {
		t.Fatal(err)
	}
	vc, _ := entity.VerificationContent()
	sc, _ := entity.SignatureContent()
	entries, _ := entity.TlogEntries()
	tle := entries[0].TransparencyLogEntry()
	logID, _ := vs.RekorLogID()
	var legacy rekorLegacyBundle
	legacy.SignedEntryTimestamp = rekorSET(t, vs, tle)
	legacy.Payload.Body = base64.StdEncoding.EncodeToString(tle.CanonicalizedBody)
	legacy.Payload.IntegratedTime, legacy.Payload.LogIndex, legacy.Payload.LogID = tle.IntegratedTime, tle.LogIndex, logID
	rb, _ := json.Marshal(legacy)
	img, err := mutate.Append(empty.Image, mutate.Addendum{
		Layer: static.NewLayer(payload, types.MediaType("application/vnd.dev.cosign.simplesigning.v1+json")),
		Annotations: map[string]string{
			cosignSignatureAnnotation:   base64.StdEncoding.EncodeToString(sc.Signature()),
			cosignCertificateAnnotation: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: vc.Certificate().Raw})),
			cosignBundleAnnotation:      string(rb),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	tag, _ := name.NewTag(repo + ":" + strings.Replace(digest.String(), ":", "-", 1) + ".sig")
	if err := remote.Write(tag, img); err != nil {
		t.Fatal(err)
	}
}

func manifestBytes(t *testing.T, repo string, digest v1.Hash) []byte {
	t.Helper()
	d, err := name.NewDigest(repo + "@" + digest.String())
	if err != nil {
		t.Fatal(err)
	}
	desc, err := remote.Get(d)
	if err != nil {
		t.Fatal(err)
	}
	return desc.Manifest
}

// pushBundle stores a sigstore bundle as an OCI 1.1 referrer of subject.
func pushBundle(t *testing.T, repo string, subject v1.Hash, mediaType string, pb *protobundle.Bundle) {
	t.Helper()
	raw, err := protojson.Marshal(pb)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bundle.NewBundle(pb); err != nil {
		t.Fatalf("test bundle is not a valid bundle: %v", err)
	}
	var img v1.Image = empty.Image
	img = mutate.MediaType(img, types.OCIManifestSchema1)
	img = mutate.ConfigMediaType(img, types.MediaType(mediaType))
	img, err = mutate.Append(img, mutate.Addendum{Layer: static.NewLayer(raw, types.MediaType(mediaType))})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := name.NewDigest(repo + "@" + subject.String())
	desc, err := remote.Head(d)
	if err != nil {
		t.Fatal(err)
	}
	img = mutate.Subject(img, *desc).(v1.Image)
	h, _ := img.Digest()
	ref, _ := name.NewDigest(repo + "@" + h.String())
	if err := remote.Write(ref, img); err != nil {
		t.Fatal(err)
	}
}

func certBundle(t *testing.T, entity *ca.TestEntity, mediaType string, tle *protorekor.TransparencyLogEntry) *protobundle.Bundle {
	t.Helper()
	vc, _ := entity.VerificationContent()
	sc, _ := entity.SignatureContent()
	pb := &protobundle.Bundle{
		MediaType: mediaType,
		VerificationMaterial: &protobundle.VerificationMaterial{
			Content: &protobundle.VerificationMaterial_Certificate{
				Certificate: &protocommon.X509Certificate{RawBytes: vc.Certificate().Raw},
			},
			TlogEntries: []*protorekor.TransparencyLogEntry{tle},
		},
	}
	if ts, _ := entity.Timestamps(); len(ts) > 0 && mediaType != "application/vnd.dev.sigstore.bundle+json;version=0.1" {
		pb.VerificationMaterial.TimestampVerificationData = &protobundle.TimestampVerificationData{}
		for _, raw := range ts {
			pb.VerificationMaterial.TimestampVerificationData.Rfc3161Timestamps = append(
				pb.VerificationMaterial.TimestampVerificationData.Rfc3161Timestamps, &protocommon.RFC3161SignedTimestamp{SignedTimestamp: raw})
		}
	}
	if env := sc.EnvelopeContent(); env != nil {
		raw := env.RawEnvelope()
		payload, _ := base64.StdEncoding.DecodeString(raw.Payload)
		pe := &protodsse.Envelope{Payload: payload, PayloadType: raw.PayloadType}
		for _, s := range raw.Signatures {
			sig, _ := base64.StdEncoding.DecodeString(s.Sig)
			pe.Signatures = append(pe.Signatures, &protodsse.Signature{Sig: sig, Keyid: s.KeyID})
		}
		pb.Content = &protobundle.Bundle_DsseEnvelope{DsseEnvelope: pe}
		return pb
	}
	ms := sc.MessageSignatureContent()
	pb.Content = &protobundle.Bundle_MessageSignature{MessageSignature: &protocommon.MessageSignature{
		MessageDigest: &protocommon.HashOutput{Algorithm: protocommon.HashAlgorithm_SHA2_256, Digest: ms.Digest()},
		Signature:     ms.Signature(),
	}}
	return pb
}

func firstEntry(t *testing.T, entity *ca.TestEntity, kind, version string) *protorekor.TransparencyLogEntry {
	t.Helper()
	entries, _ := entity.TlogEntries()
	tle := proto.Clone(entries[0].TransparencyLogEntry()).(*protorekor.TransparencyLogEntry)
	tle.KindVersion = &protorekor.KindVersion{Kind: kind, Version: version}
	return tle
}

// signBundleV01 makes a v0.1 bundle (signed entry timestamp) with a message
// signature over the image manifest, which is what the artifact digest of an
// image is.
func signBundleV01(t *testing.T, vs *ca.VirtualSigstore, repo string, subject, signed v1.Hash, subjectName, issuer string) {
	t.Helper()
	entity, err := vs.SignWithVersion(subjectName, issuer, manifestBytes(t, repo, signed), "v0.1")
	if err != nil {
		t.Fatal(err)
	}
	tle := firstEntry(t, entity, "hashedrekord", "0.0.1")
	tle.InclusionPromise = &protorekor.InclusionPromise{SignedEntryTimestamp: rekorSET(t, vs, tle)}
	pb := certBundle(t, entity, "application/vnd.dev.sigstore.bundle+json;version=0.1", tle)
	// v0.1 bundles carry a chain.
	pb.VerificationMaterial.Content = &protobundle.VerificationMaterial_X509CertificateChain{
		X509CertificateChain: &protocommon.X509CertificateChain{Certificates: []*protocommon.X509Certificate{{RawBytes: pb.VerificationMaterial.GetCertificate().RawBytes}}},
	}
	pushBundle(t, repo, subject, "application/vnd.dev.sigstore.bundle+json;version=0.1", pb)
}

// signBundleV03 makes a v0.3 bundle: a DSSE in-toto statement about the image
// digest, logged with an inclusion proof and signed checkpoint.
func signBundleV03(t *testing.T, vs *ca.VirtualSigstore, repo string, subject v1.Hash, subjectName, issuer string) {
	t.Helper()
	statement := fmt.Sprintf(`{"_type":"https://in-toto.io/Statement/v1","subject":[{"name":%q,"digest":{"sha256":%q}}],"predicateType":"https://example.com/signed/v1","predicate":{}}`,
		repo, strings.TrimPrefix(subject.String(), "sha256:"))
	entity, err := vs.AttestAtTime(subjectName, issuer, []byte(statement), time.Now().Add(5*time.Minute), true)
	if err != nil {
		t.Fatal(err)
	}
	tle := firstEntry(t, entity, "dsse", "0.0.1")
	if tle.InclusionProof == nil {
		t.Fatal("the test log gave no inclusion proof")
	}
	pushBundle(t, repo, subject, "application/vnd.dev.sigstore.bundle.v0.3+json",
		certBundle(t, entity, "application/vnd.dev.sigstore.bundle.v0.3+json", tle))
}

func TestKeylessLegacySignatureTag(t *testing.T) {
	ctx := context.Background()
	vs, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatal(err)
	}
	repo := testRegistry(t) + "/tasks/env"
	good := pushRandom(t, repo+":good")
	signLegacyKeyless(t, vs, repo, good, good, testSubject, testIssuer)
	other := pushRandom(t, repo+":other")
	signLegacyKeyless(t, vs, repo, other, other, "someone@else.example", testIssuer)
	wrongIssuer := pushRandom(t, repo+":issuer")
	signLegacyKeyless(t, vs, repo, wrongIssuer, wrongIssuer, testSubject, "https://accounts.evil.example")
	// A valid signature of another image, copied under this image's tag.
	replayed := pushRandom(t, repo+":replayed")
	signLegacyKeyless(t, vs, repo, replayed, good, testSubject, testIssuer)
	unsigned := pushRandom(t, repo+":unsigned")

	exact := KeylessIdentity{Issuer: testIssuer, Subject: testSubject}
	v, err := newSignatureVerifierWithRoot(keylessPolicy(exact), vs)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		ref     string
		digest  v1.Hash
		wantErr string
	}{
		{repo + ":good", good, ""},
		{repo + "@" + good.String(), good, ""},
		{repo + ":other", other, "allowed identity"},
		{repo + ":issuer", wrongIssuer, "allowed identity"},
		{repo + ":replayed", replayed, "does not sign this digest"},
		{repo + ":unsigned", unsigned, "is not signed"},
	} {
		err := v.verify(ctx, tc.ref, tc.digest.String())
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%s: %v", tc.ref, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%s: got %v, want %q", tc.ref, err, tc.wantErr)
		}
	}

	// Identities by regular expression, and any of several.
	re, _ := newSignatureVerifierWithRoot(keylessPolicy(
		KeylessIdentity{IssuerRegexp: `^https://token\.actions\.example\.com$`, SubjectRegexp: `^https://github\.com/acme/images/.*@refs/tags/v.*$`},
	), vs)
	if err := re.verify(ctx, repo+":good", good.String()); err != nil {
		t.Errorf("regexp identity: %v", err)
	}
	either, _ := newSignatureVerifierWithRoot(keylessPolicy(
		KeylessIdentity{Issuer: testIssuer, Subject: "nobody@example.com"},
		KeylessIdentity{Issuer: testIssuer, Subject: "someone@else.example"},
	), vs)
	if err := either.verify(ctx, repo+":other", other.String()); err != nil {
		t.Errorf("second identity: %v", err)
	}
	if err := either.verify(ctx, repo+":good", good.String()); err == nil {
		t.Error("an identity outside the list was accepted")
	}

	// A different CA than the trusted one (another Sigstore deployment).
	rogue, _ := ca.NewVirtualSigstore()
	roguePolicy, _ := newSignatureVerifierWithRoot(keylessPolicy(exact), rogue)
	if err := roguePolicy.verify(ctx, repo+":good", good.String()); err == nil {
		t.Error("a certificate from an untrusted CA was accepted")
	}
}

func TestKeylessReferrerBundles(t *testing.T) {
	ctx := context.Background()
	vs, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatal(err)
	}
	repo := testRegistry(t) + "/tasks/env"
	exact := KeylessIdentity{Issuer: testIssuer, Subject: testSubject}
	v, err := newSignatureVerifierWithRoot(keylessPolicy(exact), vs)
	if err != nil {
		t.Fatal(err)
	}

	v01 := pushRandom(t, repo+":v01")
	signBundleV01(t, vs, repo, v01, v01, testSubject, testIssuer)
	v03 := pushRandom(t, repo+":v03")
	signBundleV03(t, vs, repo, v03, testSubject, testIssuer)
	wrongID := pushRandom(t, repo+":wrongid")
	signBundleV01(t, vs, repo, wrongID, wrongID, "someone@else.example", testIssuer)
	// A bundle signed over another image's manifest, attached to this one.
	victim, signedOther := pushRandom(t, repo+":victim"), pushRandom(t, repo+":signed")
	signBundleV01(t, vs, repo, victim, signedOther, testSubject, testIssuer)
	unsigned := pushRandom(t, repo+":unsigned")

	for _, tc := range []struct {
		name    string
		ref     string
		digest  v1.Hash
		wantErr string
	}{
		{"v0.1 bundle, signed entry timestamp", repo + ":v01", v01, ""},
		{"v0.3 DSSE bundle, inclusion proof and checkpoint", repo + ":v03", v03, ""},
		{"by another identity", repo + ":wrongid", wrongID, "allowed identity"},
		{"signed over a different image", repo + ":victim", victim, "allowed identity"},
		{"no bundle", repo + ":unsigned", unsigned, "no sigstore bundle refers to"},
	} {
		err := v.verify(ctx, tc.ref, tc.digest.String())
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%s: got %v, want %q", tc.name, err, tc.wantErr)
		}
	}

	// A log entry the trusted Rekor did not sign: a bundle from another
	// deployment's log is refused even with a valid certificate chain.
	rogueLog := pushRandom(t, repo+":roguelog")
	rogue, _ := ca.NewVirtualSigstore()
	signBundleV01(t, rogue, repo, rogueLog, rogueLog, testSubject, testIssuer)
	if err := v.verify(ctx, repo+":roguelog", rogueLog.String()); err == nil {
		t.Error("a bundle from an untrusted Sigstore was accepted")
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
}

// A key policy and a keyless policy together: either kind of signature passes.
func TestKeyAndKeylessTogether(t *testing.T) {
	ctx := context.Background()
	vs, _ := ca.NewVirtualSigstore()
	repo := testRegistry(t) + "/tasks/env"
	key, pemKey := newKey(t)
	byKey := pushRandom(t, repo+":bykey")
	sign(t, repo, byKey, byKey, key)
	keyless := pushRandom(t, repo+":keyless")
	signLegacyKeyless(t, vs, repo, keyless, keyless, testSubject, testIssuer)
	p := keylessPolicy(KeylessIdentity{Issuer: testIssuer, Subject: testSubject})
	p.Keys = []string{pemKey}
	v, err := newSignatureVerifierWithRoot(p, vs)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		tag    string
		digest v1.Hash
	}{{"bykey", byKey}, {"keyless", keyless}} {
		if err := v.verify(ctx, repo+":"+tc.tag, tc.digest.String()); err != nil {
			t.Errorf("%s: %v", tc.tag, err)
		}
	}
}

func TestKeylessPolicyConfig(t *testing.T) {
	dir := t.TempDir()
	notJSON := filepath.Join(dir, "root.json")
	if err := os.WriteFile(notJSON, []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]SignaturePolicy{
		"no trusted root":     {Keyless: []KeylessIdentity{{Issuer: "i", Subject: "s"}}},
		"missing root file":   {Keyless: []KeylessIdentity{{Issuer: "i", Subject: "s"}}, TrustedRoot: "/no/such/trusted_root.json"},
		"malformed root":      {Keyless: []KeylessIdentity{{Issuer: "i", Subject: "s"}}, TrustedRoot: notJSON},
		"malformed root json": {Keyless: []KeylessIdentity{{Issuer: "i", Subject: "s"}}, TrustedRoot: "{not json"},
		"nothing":             {},
	} {
		if err := (Config{ImageSignatures: &p}).Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	for name, id := range map[string]KeylessIdentity{
		"no issuer":       {Subject: "s"},
		"no subject":      {Issuer: "i"},
		"both issuers":    {Issuer: "i", IssuerRegexp: ".*", Subject: "s"},
		"both subjects":   {Issuer: "i", Subject: "s", SubjectRegexp: ".*"},
		"invalid regexp":  {Issuer: "i", SubjectRegexp: "("},
		"invalid regexp2": {IssuerRegexp: "(", Subject: "s"},
	} {
		if _, err := id.certificateIdentity(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The legacy bundle annotation round-trips what the verifier expects, and a
// malformed one is refused rather than ignored.
func TestLegacyKeylessBundleParsing(t *testing.T) {
	payload := []byte("payload")
	for name, a := range map[string]map[string]string{
		"no certificate": {cosignBundleAnnotation: `{}`},
		"no bundle":      {cosignCertificateAnnotation: "-----BEGIN CERTIFICATE-----\nAA==\n-----END CERTIFICATE-----\n"},
	} {
		if _, err := legacyKeylessBundle(a, payload, []byte("sig")); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
