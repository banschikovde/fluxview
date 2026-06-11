# fluxview

CLI tool for building, diffing, and validating Flux GitOps resources locally. Works with a local git repository — no cluster connection required. All tools (git, kustomize, helm) are embedded via Go SDK; no external binaries needed.

## Features

- **build** — assemble Kustomization and HelmRelease resources with Helm chart inflation
- **diff** — per-resource comparison against a git revision (flux-local style)
- **validate** — validate resources against CRD schemas (Flux CRDs + any custom)
- Parallel builds for diff, single-pass YAML processing
- Automatic secret redaction
- postBuild variable substitution from ConfigMaps
- Recursive Kustomization discovery (Flux controller behavior)

## Installation

### From source

```bash
go install github.com/banschikovde/fluxview/cmd/fluxview@latest
```

### Docker

Pre-built image from GitHub Container Registry:

```bash
docker pull ghcr.io/banschikovde/fluxview:latest
```

Tags: `:latest`, plus a tag per release (version-pinned).

Run against a local repo mounted as a volume:

```bash
docker run --rm -v $(pwd):/repo -w /repo ghcr.io/banschikovde/fluxview:latest \
  diff ks --path clusters/prod/ --branch-orig master --strip-attrs --skip-crds
```

Build locally:

```bash
docker build -t fluxview .
```

CRD schemas for `validate` are not bundled — mount them via `-v /path/to/crds:/crds`:

```bash
docker run --rm -v $(pwd):/repo -v /path/to/crds:/crds \
  -w /repo ghcr.io/banschikovde/fluxview:latest \
  validate ks --path clusters/prod/
```

## Commands

### build — assemble resources

```bash
# Build all Kustomizations (with HelmRelease inflation)
fluxview build ks --path clusters/prod/

# Build only resources in flux-system namespace
fluxview build ks --path clusters/prod/ --namespace flux-system

# Build without CRDs and noisy metadata attributes
fluxview build ks --path clusters/prod/ --skip-crds --strip-attrs

# Inflate a specific HelmRelease
fluxview build hr podinfo --path clusters/prod/
```

### diff — compare changes

```bash
# Diff against master
fluxview diff ks --path clusters/prod/ --branch-orig master

# Diff only resources in flux-system namespace
fluxview diff ks --path clusters/prod/ --branch-orig master --namespace flux-system

# With flux-local flags
fluxview diff ks --path clusters/prod/ --branch-orig master \
  --strip-attrs --skip-crds --unified 6

# Diff a HelmRelease
fluxview diff hr podinfo --path clusters/prod/
```

Diff output is per-resource — each changed resource gets its own header followed by a line-level diff. In a TTY, changes are color-coded: green for added, red for removed. In pipes/CI, `+`/`-` prefixes are used.

```
---------------------------------------
 VMCluster: victoria-metrics/vmcluster
---------------------------------------
   name: vmcluster
   namespace: victoria-metrics
 spec:
+  clusterVersion: v1.146.0-cluster
-  clusterVersion: v1.145.0-cluster
   replicationFactor: 2
```

### validate — validate resources

```bash
# Validate against CRD schemas (defaults to /crds/ or ./crds/)
fluxview validate ks --path clusters/prod/

# Specify schema directory
fluxview validate ks --path clusters/prod/ --schema-dir /crds
```

Two schema formats are supported:
- **JSON Schema** (`.json`) — kubeconform-compatible (e.g., `crd-schemas.tar.gz` from flux2 releases)
- **CRD YAML** (`.yaml`/`.yml`) — standard Kubernetes CustomResourceDefinition manifests

Missing schemas never break the pipeline — resources without a matching schema are silently skipped.

## Flags

| Flag | Commands | Description |
|------|----------|-------------|
| `-p, --path` | build, diff, validate | Path to cluster directory |
| `-n, --namespace` | build, diff, validate | Filter resources by namespace (default: all) |
| `--branch-orig` | diff | Branch/revision to compare against (default: auto-detect) |
| `--color` | diff | Color mode: `auto`, `always`, `never` |
| `--unified` | diff | Context lines (default: 3) |
| `--skip-crds` | build, diff | Skip CustomResourceDefinition resources |
| `--strip-attrs` | build, diff | Strip creationTimestamp, status, uid, etc. |
| `--schema-dir` | validate | Schema files directory |

## Exit codes

| Code | Meaning |
|------|---------|
| 0 | Success / no differences / all resources valid |
| 1 | Differences found (diff only) |
| 2 | Error |

## CRD schemas

CRD schemas for `validate` are loaded from `--schema-dir` (default: `/crds/` or `./crds/`). Download Flux CRD schemas:

```bash
wget -qO- "https://github.com/fluxcd/flux2/releases/download/v2.9.1/crd-schemas.tar.gz" | tar xzf - -C ./crds
```

For custom CRDs (VictoriaMetrics, Kyverno, etc.), place YAML files alongside.

## CI

Example GitLab CI:

```yaml
fluxview:diff:
  image: ghcr.io/banschikovde/fluxview:latest
  script:
    - fluxview diff ks --path clusters/prod/ --branch-orig master
        --strip-attrs --skip-crds --color never
  rules:
    - if: $CI_MERGE_REQUEST_ID
```

## Technology

- Go, Cobra CLI
- go-git (Git SDK)
- kustomize SDK (build)
- Helm SDK (inflation)
- k8s.io/apiextensions-apiserver (CRD validation)

## License

Apache-2.0
