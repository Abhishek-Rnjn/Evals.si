#!/usr/bin/env bash
# Verifies a release's signatures: the images and the OCI charts were signed by
# this repository's release workflow (keyless cosign, GitHub's OIDC identity).
#
#   scripts/verify-release.sh 0.5.0 [owner]
#
# Needs cosign. Prints what it checked; exits non-zero on the first failure.
set -euo pipefail

version="${1:?usage: verify-release.sh VERSION [OWNER]}"
owner="${2:-abhishek-rnjn}"
repo="${REPO:-Abhishek-Rnjn/Evals.si}"

identity="https://github.com/${repo}/.github/workflows/release.yml@refs/tags/v${version}"
issuer=https://token.actions.githubusercontent.com

verify() {
  echo "== $1"
  cosign verify --certificate-identity "$identity" --certificate-oidc-issuer "$issuer" "$1" >/dev/null
}

for ref in \
  "ghcr.io/${owner}/evalsi:${version}" \
  "ghcr.io/${owner}/evalsi-collector:${version}" \
  "ghcr.io/${owner}/charts/evalsi:${version}" \
  "ghcr.io/${owner}/charts/evalsi-crds:${version}" \
  "ghcr.io/${owner}/charts/evalsi-sandboxd:${version}"; do
  verify "$ref"
done

# The SBOM is attested, not only signed: show that it is there.
echo "== SBOM attestation of ghcr.io/${owner}/evalsi:${version}"
cosign verify-attestation --type spdxjson --certificate-identity "$identity" --certificate-oidc-issuer "$issuer" \
  "ghcr.io/${owner}/evalsi:${version}" >/dev/null
echo "all signatures verified for ${version}"
