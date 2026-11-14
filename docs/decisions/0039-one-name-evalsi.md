# 0039. One name: Evalsi

- **Status:** Accepted, 2026-11-14. Amends [0006](0006-naming-and-namespaces.md).

## Context

The project was published under two names. The Python package, the CLI, the
daemon, the images, the charts and the website (`evalsi.dev`) said `evalsi`;
the display name, the GitHub repository, the Go module and the Kubernetes API
group said `Evals.si` or `evals.si`. People could not tell what to search for,
and the two spellings appeared side by side on every page.

Before 0.4.0 nobody had installed a release, so no user's spec files, clusters
or identity-provider settings depended on the old names.

## Decision

Everything is named Evalsi, from 0.4.0 on.

| Thing | Before | From 0.4.0 |
|-------|--------|------------|
| Display name | Evals.si | Evalsi |
| GitHub repository | `Abhishek-Rnjn/Evals.si` | `Abhishek-Rnjn/evalsi` |
| Go module | `github.com/abhishek-rnjn/evals.si` | `github.com/abhishek-rnjn/evalsi` |
| Kubernetes API group | `evals.si` | `evalsi.dev` |
| Spec files | `apiVersion: evals.si/v1alpha1` | `apiVersion: evalsi.dev/v1alpha1` |
| Label and annotation prefix | `evals.si/` (for example `evals.si/project`) | `evalsi.dev/` |
| CRD names | `evalruns.evals.si` and so on | `evalruns.evalsi.dev` and so on |
| Admission webhooks | `validate.evals.si`, `/validate-evals-si` | `validate.evalsi.dev`, `/validate-evalsi-dev` |
| Default token audience | `evals.si` | `evalsi.dev` |

Unchanged, because they already said `evalsi`: the PyPI packages and the
import name, the `evalsi` CLI, `evalsid`, the protobuf packages
(`evalsi.v1alpha1`), the images, the charts, `evalsi.yaml` and the website.

The API group is `evalsi.dev`, not `evalsi`: Kubernetes requires a
CustomResourceDefinition's group to contain a dot. `evalsi.dev` is the
project's own domain, which is the usual choice.

## Consequences

- There is no compatibility path. A spec file with `apiVersion:
  evals.si/v1alpha1` is refused, and objects of the old CRDs are a different
  resource type. That is acceptable only because no release had been
  installed; a rename like this after users exist needs both groups accepted
  for a deprecation period.
- Releases up to 0.3.2 were signed by the workflow of `Abhishek-Rnjn/Evals.si`;
  `scripts/verify-release.sh` checks those with `REPO=Abhishek-Rnjn/Evals.si`.
- PyPI trusted publishers are registered against the repository name, so they
  must be registered again for `Abhishek-Rnjn/evalsi` before the first release
  from the renamed repository.
- Records written before the rename (verification reports, reviews, earlier
  decisions and released changelog sections) keep the names they were written
  with.
