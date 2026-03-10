# Release Automation Design

## Overview

Add GitHub release automation to mcp-dap-server using GoReleaser OSS, with sigstore/cosign signing for binaries and container images published to ghcr.io.

## Approach

GoReleaser OSS with built-in Docker and cosign signing support. A single `.goreleaser.yaml` drives everything: binary builds, archives, Docker images, and signing. GitHub Actions workflow sets up the environment and calls `goreleaser release`.

## Version Injection

A `var version = "dev"` in `main.go` is overridden at build time via ldflags: `-s -w -X main.version={{.Version}}`. During development it shows `"dev"`, in release builds it shows the git tag version (e.g., `1.2.0`).

## Build Targets

Binaries for:
- linux/amd64, linux/arm64
- darwin/amd64, darwin/arm64
- windows/amd64

Archives: tar.gz for Linux/macOS, zip for Windows. Include LICENSE and README.md.

## Container Images

Two variants, both for linux/amd64 and linux/arm64:

### Minimal (`ghcr.io/go-delve/mcp-dap-server`)
- Base: `gcr.io/distroless/static-debian12`
- Contains only the mcp-dap-server binary
- Dockerfile: `Dockerfile.minimal`
- Tags: `latest`, `{{.Version}}`, `{{.Major}}.{{.Minor}}`

### Batteries-included (`ghcr.io/go-delve/mcp-dap-server-debug`)
- Base: Debian bookworm-slim
- Includes: Delve, GDB, OpenDebugAD7 (latest cpptools release)
- Dockerfile: `Dockerfile.debug` (multi-stage)
- Stage 1 (Go builder): installs Delve via `go install`
- Stage 2 (Debian final): installs GDB via apt, downloads OpenDebugAD7, copies Delve and mcp-dap-server binary
- Sets `MCP_DAP_CPPTOOLS_PATH` environment variable
- Tags: same pattern as minimal

## Signing

- Binary checksums: cosign keyless signing via sigstore OIDC
- Container images: cosign keyless signing via sigstore OIDC
- Configured via GoReleaser `signs:` and `docker_signs:` sections

## GitHub Actions Workflow

File: `.github/workflows/release.yml`

### Triggers
- `push` with tag filter `v*` (tag-triggered releases)
- `workflow_dispatch` with a `version` input (manual releases)

### Permissions
- `contents: write` — create GitHub release
- `packages: write` — push to ghcr.io
- `id-token: write` — keyless cosign signing via sigstore OIDC

### Steps
1. Checkout code
2. Set up Go 1.24
3. Log in to ghcr.io with GITHUB_TOKEN
4. Set up QEMU (cross-platform Docker builds)
5. Set up Docker Buildx
6. Install cosign
7. If manual dispatch: create and push the git tag
8. Run GoReleaser via `goreleaser/goreleaser-action@v6`

## Files Changed

| File | Action | Purpose |
|------|--------|---------|
| `main.go` | Edit | Add `var version = "dev"`, use in Implementation |
| `.goreleaser.yaml` | Create | GoReleaser config |
| `Dockerfile.minimal` | Create | Distroless image |
| `Dockerfile.debug` | Create | Debian image with debuggers |
| `.github/workflows/release.yml` | Create | Release workflow |
| `.gitignore` | Edit | Add `dist/` |

## Changelog

Auto-generated from commits, grouped by conventional commit type.
