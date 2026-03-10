# Release Automation Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add GitHub release automation with GoReleaser, sigstore signing, and container images.

**Architecture:** GoReleaser OSS drives binary builds, Docker image creation, and cosign signing from a single config file. A GitHub Actions workflow triggers on tag push or manual dispatch, sets up the environment, and invokes GoReleaser.

**Tech Stack:** GoReleaser OSS, cosign/sigstore, Docker (multi-platform via QEMU/Buildx), GitHub Actions, ghcr.io

**Spec:** `docs/superpowers/specs/2026-03-10-release-automation-design.md`

---

## Chunk 1: Version Injection and Project Prep

### Task 1: Add version variable and update .gitignore

**Files:**
- Modify: `main.go:1-46`
- Modify: `.gitignore`

- [ ] **Step 1: Add version variable to main.go**

Add a package-level variable before `main()` and update the Implementation struct:

```go
var version = "dev"
```

In the `main()` function, change line 36 from:
```go
Version: "v1.0.0",
```
to:
```go
Version: version,
```

- [ ] **Step 2: Add dist/ to .gitignore**

Append `dist/` to `.gitignore`. This is GoReleaser's default output directory.

- [ ] **Step 3: Verify the build still works**

Run: `go build -o /dev/null .`
Expected: Successful build, exit code 0.

- [ ] **Step 4: Verify tests still pass**

Run: `go test -v -count=1 ./...`
Expected: All tests pass.

- [ ] **Step 5: Commit**

```bash
git add main.go .gitignore
git commit -m "feat: add build-time version injection for GoReleaser"
```

---

## Chunk 2: Dockerfiles

### Task 2: Create Dockerfile.minimal

**Files:**
- Create: `Dockerfile.minimal`

- [ ] **Step 1: Create the minimal Dockerfile**

```dockerfile
FROM gcr.io/distroless/static-debian12:nonroot
COPY mcp-dap-server /usr/local/bin/mcp-dap-server
ENTRYPOINT ["mcp-dap-server"]
```

Notes:
- Uses `nonroot` tag for security best practice.
- GoReleaser copies the pre-built binary into the build context, so no multi-stage build is needed.
- No `EXPOSE` since mcp-dap-server uses stdio transport by default.

- [ ] **Step 2: Commit**

```bash
git add Dockerfile.minimal
git commit -m "feat: add minimal container image (distroless)"
```

### Task 3: Create Dockerfile.debug

**Files:**
- Create: `Dockerfile.debug`

- [ ] **Step 1: Create the batteries-included Dockerfile**

This is a multi-stage Dockerfile:

```dockerfile
# Stage 1: Install Delve
FROM golang:1.24-bookworm AS delve-builder
RUN go install github.com/go-delve/delve/cmd/dlv@latest

# Stage 2: Final image with all debugging tools
FROM debian:bookworm-slim

# Install GDB and dependencies
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        gdb \
        curl \
        ca-certificates \
        unzip \
    && rm -rf /var/lib/apt/lists/*

# Install OpenDebugAD7 from cpptools release
RUN ARCH=$(dpkg --print-architecture) && \
    if [ "$ARCH" = "amd64" ]; then VSIX_ARCH="linux-x64"; \
    elif [ "$ARCH" = "arm64" ]; then VSIX_ARCH="linux-aarch64"; \
    else echo "Unsupported architecture: $ARCH" && exit 1; fi && \
    LATEST_URL=$(curl -s https://api.github.com/repos/microsoft/vscode-cpptools/releases/latest \
        | grep "browser_download_url.*cpptools-${VSIX_ARCH}.vsix" \
        | head -1 | cut -d '"' -f 4) && \
    curl -fSL -o /tmp/cpptools.vsix "$LATEST_URL" && \
    mkdir -p /opt/cpptools && \
    unzip -q /tmp/cpptools.vsix -d /opt/cpptools && \
    chmod +x /opt/cpptools/extension/debugAdapters/bin/OpenDebugAD7 && \
    rm /tmp/cpptools.vsix

# Copy Delve from builder
COPY --from=delve-builder /go/bin/dlv /usr/local/bin/dlv

# Copy mcp-dap-server binary (provided by GoReleaser)
COPY mcp-dap-server /usr/local/bin/mcp-dap-server

# Set cpptools path so mcp-dap-server can find OpenDebugAD7
ENV MCP_DAP_CPPTOOLS_PATH=/opt/cpptools/extension/debugAdapters/bin/OpenDebugAD7

ENTRYPOINT ["mcp-dap-server"]
```

Notes:
- Multi-arch support via `dpkg --print-architecture` to select the correct cpptools VSIX.
- Delve is built from source in a Go builder stage to match the target architecture.
- curl and unzip are needed for cpptools download but remain in the image for potential debugging use.

- [ ] **Step 2: Commit**

```bash
git add Dockerfile.debug
git commit -m "feat: add debug container image with Delve, GDB, and cpptools"
```

---

## Chunk 3: GoReleaser Configuration

### Task 4: Create .goreleaser.yaml

**Files:**
- Create: `.goreleaser.yaml`

- [ ] **Step 1: Create the GoReleaser configuration**

```yaml
version: 2

before:
  hooks:
    - go mod tidy

builds:
  - id: mcp-dap-server
    binary: mcp-dap-server
    env:
      - CGO_ENABLED=0
    goos:
      - linux
      - darwin
      - windows
    goarch:
      - amd64
      - arm64
    ignore:
      - goos: windows
        goarch: arm64
    ldflags:
      - -s -w -X main.version={{.Version}}

archives:
  - id: default
    formats:
      - tar.gz
    format_overrides:
      - goos: windows
        formats:
          - zip
    files:
      - LICENSE
      - README.md

dockers:
  # Minimal image - amd64
  - id: minimal-amd64
    ids: [mcp-dap-server]
    goos: linux
    goarch: amd64
    image_templates:
      - "ghcr.io/go-delve/mcp-dap-server:{{ .Version }}-amd64"
    dockerfile: Dockerfile.minimal
    use: buildx
    build_flag_templates:
      - "--platform=linux/amd64"

  # Minimal image - arm64
  - id: minimal-arm64
    ids: [mcp-dap-server]
    goos: linux
    goarch: arm64
    image_templates:
      - "ghcr.io/go-delve/mcp-dap-server:{{ .Version }}-arm64"
    dockerfile: Dockerfile.minimal
    use: buildx
    build_flag_templates:
      - "--platform=linux/arm64"

  # Debug image - amd64
  - id: debug-amd64
    ids: [mcp-dap-server]
    goos: linux
    goarch: amd64
    image_templates:
      - "ghcr.io/go-delve/mcp-dap-server-debug:{{ .Version }}-amd64"
    dockerfile: Dockerfile.debug
    use: buildx
    build_flag_templates:
      - "--platform=linux/amd64"

  # Debug image - arm64
  - id: debug-arm64
    ids: [mcp-dap-server]
    goos: linux
    goarch: arm64
    image_templates:
      - "ghcr.io/go-delve/mcp-dap-server-debug:{{ .Version }}-arm64"
    dockerfile: Dockerfile.debug
    use: buildx
    build_flag_templates:
      - "--platform=linux/arm64"

docker_manifests:
  # Minimal image manifests
  - name_template: "ghcr.io/go-delve/mcp-dap-server:{{ .Version }}"
    image_templates:
      - "ghcr.io/go-delve/mcp-dap-server:{{ .Version }}-amd64"
      - "ghcr.io/go-delve/mcp-dap-server:{{ .Version }}-arm64"
  - name_template: "ghcr.io/go-delve/mcp-dap-server:{{ .Major }}.{{ .Minor }}"
    image_templates:
      - "ghcr.io/go-delve/mcp-dap-server:{{ .Version }}-amd64"
      - "ghcr.io/go-delve/mcp-dap-server:{{ .Version }}-arm64"
  - name_template: "ghcr.io/go-delve/mcp-dap-server:latest"
    image_templates:
      - "ghcr.io/go-delve/mcp-dap-server:{{ .Version }}-amd64"
      - "ghcr.io/go-delve/mcp-dap-server:{{ .Version }}-arm64"

  # Debug image manifests
  - name_template: "ghcr.io/go-delve/mcp-dap-server-debug:{{ .Version }}"
    image_templates:
      - "ghcr.io/go-delve/mcp-dap-server-debug:{{ .Version }}-amd64"
      - "ghcr.io/go-delve/mcp-dap-server-debug:{{ .Version }}-arm64"
  - name_template: "ghcr.io/go-delve/mcp-dap-server-debug:{{ .Major }}.{{ .Minor }}"
    image_templates:
      - "ghcr.io/go-delve/mcp-dap-server-debug:{{ .Version }}-amd64"
      - "ghcr.io/go-delve/mcp-dap-server-debug:{{ .Version }}-arm64"
  - name_template: "ghcr.io/go-delve/mcp-dap-server-debug:latest"
    image_templates:
      - "ghcr.io/go-delve/mcp-dap-server-debug:{{ .Version }}-amd64"
      - "ghcr.io/go-delve/mcp-dap-server-debug:{{ .Version }}-arm64"

signs:
  - cmd: cosign
    artifacts: checksum
    args:
      - "sign-blob"
      - "--yes"
      - "${artifact}"
      - "--output-signature=${signature}"
      - "--output-certificate=${certificate}"

docker_signs:
  - cmd: cosign
    artifacts: manifests
    args:
      - "sign"
      - "--yes"
      - "${artifact}"

checksum:
  name_template: "checksums.txt"

changelog:
  sort: asc
  groups:
    - title: Features
      regexp: '^.*?feat(\([[:word:]]+\))??!?:.+$'
      order: 0
    - title: Bug fixes
      regexp: '^.*?fix(\([[:word:]]+\))??!?:.+$'
      order: 1
    - title: Documentation
      regexp: '^.*?docs(\([[:word:]]+\))??!?:.+$'
      order: 2
    - title: Other
      order: 999

release:
  github:
    owner: go-delve
    name: mcp-dap-server
```

- [ ] **Step 2: Validate the configuration**

Run: `goreleaser check`
Expected: No errors. (If goreleaser is not installed locally, skip — CI will validate.)

If goreleaser is not installed, verify YAML syntax:
Run: `python3 -c "import yaml; yaml.safe_load(open('.goreleaser.yaml'))"`
Expected: No errors.

- [ ] **Step 3: Commit**

```bash
git add .goreleaser.yaml
git commit -m "feat: add GoReleaser configuration for release automation"
```

---

## Chunk 4: GitHub Actions Workflow

### Task 5: Create release workflow

**Files:**
- Create: `.github/workflows/release.yml`

- [ ] **Step 1: Create the release workflow**

```yaml
name: Release

on:
  push:
    tags:
      - "v*"
  workflow_dispatch:
    inputs:
      version:
        description: "Release version (e.g., v1.2.3)"
        required: true
        type: string

permissions:
  contents: write
  packages: write
  id-token: write

jobs:
  release:
    runs-on: ubuntu-latest
    steps:
      - name: Checkout
        uses: actions/checkout@v4
        with:
          fetch-depth: 0

      - name: Set up Go
        uses: actions/setup-go@v5
        with:
          go-version: "1.24"

      - name: Log in to ghcr.io
        uses: docker/login-action@v3
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}

      - name: Set up QEMU
        uses: docker/setup-qemu-action@v3

      - name: Set up Docker Buildx
        uses: docker/setup-buildx-action@v3

      - name: Install cosign
        uses: sigstore/cosign-installer@v3

      - name: Create and push tag (manual dispatch)
        if: github.event_name == 'workflow_dispatch'
        run: |
          git config user.name "${{ github.actor }}"
          git config user.email "${{ github.actor }}@users.noreply.github.com"
          git tag -a "${{ inputs.version }}" -m "Release ${{ inputs.version }}"
          git push origin "${{ inputs.version }}"

      - name: Run GoReleaser
        uses: goreleaser/goreleaser-action@v6
        with:
          version: "~> v2"
          args: release --clean
        env:
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
```

Notes:
- `fetch-depth: 0` is required for GoReleaser to generate changelogs from commit history.
- `setup-go@v5` (not v4 as in the existing CI) for latest features.
- QEMU and Buildx enable cross-platform Docker image builds.
- Cosign uses keyless signing via OIDC — the `id-token: write` permission enables this.
- For manual dispatch, the tag is created and pushed before GoReleaser runs.

- [ ] **Step 2: Commit**

```bash
git add .github/workflows/release.yml
git commit -m "feat: add GitHub Actions release workflow"
```

---

## Chunk 5: Final Validation

### Task 6: Dry-run validation and final commit

- [ ] **Step 1: Run the full test suite to ensure nothing is broken**

Run: `go test -v -count=1 ./...`
Expected: All tests pass.

- [ ] **Step 2: Verify the build with version ldflags**

Run: `go build -ldflags "-s -w -X main.version=0.0.0-test" -o /tmp/mcp-dap-server-test .`
Expected: Successful build, exit code 0.

- [ ] **Step 3: Clean up test binary**

Run: `rm /tmp/mcp-dap-server-test`

- [ ] **Step 4: Review all changes since starting**

Run: `git log --oneline master~5..HEAD`
Expected: See commits for version injection, Dockerfiles, GoReleaser config, and release workflow.
