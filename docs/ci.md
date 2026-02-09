# CI + Image Publishing

This repo includes GitHub Actions workflows for:
- Go build/test + kustomize validation
- building/publishing container images to GHCR

## Workflows

- `/.github/workflows/ci.yaml`
  - `gofmt` check
  - `go vet`
  - `go test`
  - `kubectl kustomize` on `k8s/` and overlays

- `/.github/workflows/images.yaml`
  - builds images from `images/*/Dockerfile`
  - pushes to GHCR on `main`
  - tags:
    - `:latest`
    - `:sha-<full_sha>`

## Default Image Names

Manifests in `k8s/` default to GHCR images under `ghcr.io/haasonsaas/`:
- `workspaces-operator`
- `workspaces-capability-broker`
- `workspaces-egress-proxy`
- `workspaces-github-webhook`
- `workspaces-agent-runner`
- `workspaces-desktop`
- `workspaces-auditship`

Forks should override images (or change the defaults) to match their GHCR org/owner.

## Production Recommendation: Pin By Digest

For supply-chain stability, pin images by digest:
- `ghcr.io/<owner>/<image>@sha256:<digest>`

This is compatible with the optional admission guardrails:
- `k8s/optional/hardened/agentjob-validatingadmissionpolicy.yaml`
- `k8s/optional/hardened/desktop-validatingadmissionpolicy.yaml`

