# Security

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub: **Security → Report a
vulnerability** on this repository. Do not open a public issue for anything
that could be exploited. Include the affected commit or image digest, how to
reproduce, and the impact you expect.

Only `main` and the newest image built from it are supported. Images are
published as `ghcr.io/abd-ulbasit/steward:sha-<commit>` (and `X.Y.Z` for
`v*` tags); `latest` moves with `main`, so deploy by digest or by an immutable
tag.

## What the pipeline checks

| Check | Where | Catches | Does not catch |
|---|---|---|---|
| golangci-lint (errcheck, govet, staticcheck, noctx, bodyclose, ...) | `ci.yml` lint | correctness and API-misuse bugs | security-specific patterns; there is no gosec or CodeQL |
| `make test` (unit + envtest) | `ci.yml` test | regressions in reconcile, provider, webhook logic | RBAC problems: envtest runs as cluster-admin |
| `make verify-generated` | `ci.yml` | CRD, RBAC, webhook and deepcopy output drifting from the code markers; unformatted code; untidy `go.mod` | — |
| govulncheck | `ci.yml`, weekly in `rescan.yml` | known Go vulnerabilities whose code is reachable, in modules and the standard library | vulnerabilities without an advisory; unreachable ones (by design) |
| Trivy image scan | `ci.yml`: the linux/amd64 and linux/arm64 images, both before the push; `rescan.yml`: both platforms of the published `latest`, weekly | fixable HIGH/CRITICAL CVEs in the image's OS packages and Go binary | unfixed CVEs and MEDIUM/LOW (not gating); misconfiguration of the Kubernetes manifests in `config/`. The scanned images are rebuilt for the push from the same commit and layer cache, so the scan covers the same contents, not the same digest |
| Kind e2e | `ci.yml` e2e (the image push waits for it) | the manager starting, metrics authz, webhook CA injection | the Application lifecycle against real CNPG/Redis/RabbitMQ (that is `hack/validate-e2e.sh`, run by hand) |
| Dependabot | `.github/dependabot.yml` | outdated Go modules, Actions SHA pins, base image digests in `Dockerfile` and `.devcontainer/Dockerfile` | the manual pins listed below |

Not covered anywhere in CI: secret scanning (rely on GitHub secret scanning and
push protection), IaC or Kubernetes manifest scanning, and image signature
verification at deploy time. Images carry a signed SLSA build provenance
attestation (`gh attestation verify oci://ghcr.io/abd-ulbasit/steward@<digest>
--owner abd-ulbasit`) and a buildx SBOM, but nothing in this repository
enforces verification.

## Pins Dependabot does not update

These versions are written into scripts, workflow inputs and the Makefile,
where no Dependabot ecosystem looks. The maintainer reviews them once a month,
and at once when the weekly rescan or an advisory names one of them. Each
download that has a checksum is verified against it; update the version and
the checksums together.

| Pin | Where | Notes |
|---|---|---|
| kind v0.33.0 (SHA-256 per arch) | `ci.yml` e2e job, `.devcontainer/post-install.sh` | checksums from the release's `.sha256sum` files |
| kubebuilder v4.16.0 (SHA-256 per arch) | `.devcontainer/post-install.sh` | checksums from the release's checksum file |
| kubectl v1.35.9 (SHA-256 per arch) | `.devcontainer/post-install.sh` | keep the minor version in step with `k8s.io/*` in `go.mod` |
| govulncheck v1.9.0 | `ci.yml`, `rescan.yml` | fetched with `go run`, so the Go checksum database verifies it |
| Trivy v0.75.0 | `version` input of trivy-action in `ci.yml` and `rescan.yml` | Dependabot bumps the action's SHA, not this input |
| golangci-lint v2.14.0 | `GOLANGCI_LINT_VERSION` in `Makefile`, `version` in `ci.yml` | keep the two equal |
| kustomize, controller-gen | `Makefile` | installed with `go install` |
| cert-manager, CloudNativePG, Prometheus Operator CRDs | `Makefile` (`dev-setup`) | applied by URL with no checksum; development clusters only |

Renovate's regex managers could track most of these automatically; that
needs the Renovate GitHub App installed on the repository.

## Known design limits

See [docs/DESIGN-DECISIONS.md](docs/DESIGN-DECISIONS.md) section 7: the
operator's RBAC is cluster-wide, and `env`/`envFrom` let anyone who can edit an
Application read any Secret in its namespace. Both have a recorded design and
are not fixed yet.
