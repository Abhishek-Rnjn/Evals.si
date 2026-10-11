# Security policy

## Reporting a vulnerability

Please report vulnerabilities privately. Do not open a public issue or pull request.

Use GitHub's private vulnerability reporting: open the repository's **Security** tab and choose **Report a vulnerability**
([direct link](https://github.com/Abhishek-Rnjn/Evals.si/security/advisories/new)).

Include what you found, the version or commit, how to reproduce it, and what an attacker gains. A proof of concept helps.

You can expect an acknowledgement within 5 working days. We will keep you informed while we confirm and fix the problem, and credit you in the advisory unless you ask us not to.

## What is in scope

Evals.si runs untrusted code (evaluators, benchmark tasks and agents) in sandboxes, and it accepts traces and API calls from other systems, so these matter most:

- Escapes from a sandbox, or a sandbox running with weaker isolation than the one requested.
- Authentication or authorization bypass, including between projects.
- Leaking credentials, tokens or secrets through logs, traces, sinks or API responses.
- Server-side request forgery, injection, or unsafe deserialization in the server, operator or worker.
- Weaknesses in the Helm charts, the operator or the air-gapped bundle that weaken a cluster's security.
- Tampering with release artifacts, or signatures that fail to verify.

Sandbox limits and what each isolation level does and does not protect against are described in [docs/DESIGN.md](docs/DESIGN.md#13-sandboxing-firecracker-bubblewrap-hardened-pods) and [decision 0005](docs/decisions/0005-sandbox-isolation-ladder.md). A report that a lower isolation level is weaker than a higher one is expected behaviour, unless the server claims otherwise.

## Supported versions

Evals.si is pre-1.0. Security fixes go into the latest release only.

| Version | Supported |
|---------|-----------|
| latest `0.x` release | yes |
| older releases | no |

## Verifying a release

Release archives, images and charts are signed keylessly with Sigstore. [docs/RELEASING.md](docs/RELEASING.md) and `scripts/verify-release.sh` describe how to verify them.
