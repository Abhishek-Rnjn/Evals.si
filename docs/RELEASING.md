# Releasing Evals.si

The [release workflow](../.github/workflows/release.yml) builds Linux amd64 and
arm64 archives, Python distributions, multi-architecture images, three OCI
charts and an amd64 air-gapped bundle. A pushed `v*` tag publishes them. A
manual run builds without publishing. Only a maintainer may authorize a dry
run or tag push; release readiness is not authorization to publish.

## Prerequisites

Before the first release, a maintainer must check the live repository settings:

- GitHub Actions is enabled and permits the actions used by the workflow.
- The publishing jobs can obtain `packages: write` and `id-token: write`; the
  GitHub release job can obtain `contents: write`. Existing GHCR packages must
  grant this repository write access. Confirm package visibility matches the
  intended distribution audience.
- Create an environment named `release` (Settings > Environments) with
  required reviewers, and restrict deployment to tags matching `v*`. A tag run
  stops at the `approve` job until a reviewer approves; nothing is pushed to a
  registry or released before then. A dry run skips this gate and publishes
  nothing. A person cannot approve their own run if "prevent self-review" is
  on, so with one maintainer leave it off or add a second reviewer.
- Keyless signing uses GitHub OIDC and Sigstore, not a stored signing key. The
  expected certificate identity includes this repository, `release.yml`, and
  the exact release tag. The verifier must not use a wildcard identity.
- If PyPI publication is intended, register one trusted publisher for each
  package on pypi.org: this owner and repository and workflow `release.yml`,
  with environment `pypi` for `evalsi` and environment `pypi-harness` for
  `evalsi-harness`. PyPI allows only one pending publisher per repository,
  workflow and environment, so the two packages cannot share an environment.
  Create both GitHub environments with required reviewers, then set
  repository variable `PUBLISH_PYPI` to `true`. Otherwise leave it unset or
  false; wheels and sdists are still release assets.
- Review the final commit, its CI results, version and release notes. Do not
  put credentials, signing keys or private registry names in the repository.

These settings are prerequisites, not verified merely by reading the workflow.
Record who checked them and the run/commit used for the release.

## Local Gates

```bash
make check
go test -race ./...
make e2e
for version in 1.34.0 1.36.0 1.37.0; do
  EVALSI_REQUIRE_HELM=1 EVALSI_TEST_KUBE_VERSION="$version" go test -count=1 ./tests/helm
done
deploy/e2e/kind-e2e.sh
```

Also run `make adapters-check` when adapter code changes. On macOS, use
`TMPDIR=/tmp` for Go tests if the temporary directory makes Unix socket paths
too long. The version loop tests Helm capabilities and rendered configuration,
not live Kubernetes clusters at those versions.

Docker Desktop without Landlock can explicitly use `SKIP_LANDLOCK=1` for the
local kind job. This defers only Landlock and is not a complete Kubernetes
verification of a Mac run. The default amd64 kind job in CI runs the Landlock
rung; it is skipped on pull requests, so dispatch `ci` on the release commit
(`workflow_dispatch`) and read the `kubernetes` job, which must end with
`=== passed`, not the Landlock-deferred message. Do not disable the default CI
check. See the [verification record](verification/README.md#ci-postgresql-and-browser-recheck-2026-10-11).

## Maintainer Dry Run

After the readiness PR is merged, a maintainer explicitly authorized to run it
opens Actions > release > Run workflow, chooses the reviewed branch and enters
`0.1.0` (without `v`). Do not choose a tag for the dry run: publication is
controlled by the tag ref, not by the version input alone.

Wait for all applicable jobs to succeed. Download the `binaries-amd64`,
`binaries-arm64`, `python`, `charts` and `bundle` artifacts. Inspect archive
contents, smoke-test the Linux binaries on the matching architecture, check
the Python package versions and chart versions, and exercise the bundle on a
native amd64 kind host. The bundle's `platform` metadata must be `linux/amd64`.
The local arm64 bundle check is not proof of the released amd64 bundle.

A successful dry run does not prove registry pushes, keyless signatures, the
published SBOM attestation, GitHub release creation or PyPI trusted publishing:
those jobs or steps are skipped without a tag. No release is marked published
or signature-verified on dry-run evidence.

## Authorized Publication

Only after explicit approval, tag the reviewed commit and push that tag. The run
then waits at the `approve` job for a reviewer of the `release` environment:

```bash
git tag -a v0.1.0 REVIEWED_COMMIT -m "Evals.si v0.1.0"
git push origin v0.1.0
```

Wait for the tag-triggered workflow, including PyPI if enabled. The PyPI
jobs wait for the approval and for every build (archives, images, charts,
bundle), since a PyPI version can never be uploaded again; the GitHub
release job does not depend on them, so check both outcomes.
Download the release assets and validate `SHA256SUMS` from their directory:

```bash
sha256sum -c SHA256SUMS
scripts/verify-release.sh 0.1.0
```

On macOS, `shasum -a 256 -c SHA256SUMS` checks the same hashes. Run the verifier
from the checkout with cosign installed; it checks both images, all three OCI
charts and the evalsi image's SPDX SBOM attestation against the exact tag's
GitHub OIDC identity. Also verify the published wheels, chart installation,
binary versions and amd64 bundle import. Record the workflow URL, commit,
artifact digests and exact checks that passed in the verification record.

If publication or verification fails, report the failed job/artifact and keep
the release unverified. Do not move a published tag or overwrite its artifacts;
fix forward under a separately approved version.