# fluxview

CLI tool for building, diffing, and validating Flux GitOps resources locally. Works with a local git repository — no cluster connection required. All tools (git, kustomize, helm) are embedded via Go SDK; no external binaries needed.

## Features

- **build** — assemble Kustomization and HelmRelease resources
- **diff** — per-resource comparison against a git revision (flux-local style)
- **validate** — validate resources against CRD schemas (Flux CRDs + any custom)
- Recursive Kustomization discovery following `spec.path` into shared bases (Flux controller behavior)
- Source resolution with repoRoot fallback for HelmRepository/OCIRepository outside `--path`
- postBuild variable substitution from ConfigMaps
- Automatic secret redaction
- Box-header output format (per-resource, sorted by kind/namespace/name)

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
  diff ks --path clusters/prod/flux/ --branch-orig master --strip-attrs helm.sh/chart,status --skip-crds
```

Build locally:

```bash
docker build -t fluxview .
```

CRD schemas for `validate` are not bundled — mount them via `-v /path/to/crds:/crds`:

```bash
docker run --rm -v $(pwd):/repo -v /path/to/crds:/crds \
  -w /repo ghcr.io/banschikovde/fluxview:latest \
  validate --path clusters/prod/flux/
```

## Commands

### build — assemble resources

Both `build ks` and `build hr` require Flux Kustomization files in `--path` (same contract).

```bash
# Build all Kustomizations (kustomize output: Flux CRs, HelmRelease, OCIRepository, etc.)
fluxview build ks --path clusters/prod/flux/

# Filter by namespace
fluxview build ks --path clusters/prod/flux/ --namespace cert-manager

# Without CRDs and noisy metadata
fluxview build ks --path clusters/prod/flux/ --skip-crds --strip-attrs status,creationTimestamp

# Inflate all HelmReleases (renders Helm chart templates)
fluxview build hr --path clusters/prod/flux/

# Inflate a specific HelmRelease
fluxview build hr podinfo --path clusters/prod/flux/
```

Output uses per-resource box headers (same format as `diff`):

```
----------------------------------------
 HelmRelease: cert-manager/cert-manager
----------------------------------------
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
...
```

### diff — compare changes

```bash
# Diff everything (Kustomizations + HelmReleases)
fluxview diff all --path clusters/prod/flux/ --branch-orig master

# Diff all Kustomizations against master
fluxview diff ks --path clusters/prod/flux/ --branch-orig master

# Diff only resources in flux-system namespace
fluxview diff ks --path clusters/prod/flux/ --branch-orig master --namespace flux-system

# With flux-local flags
fluxview diff ks --path clusters/prod/flux/ --branch-orig master \
  --strip-attrs helm.sh/chart,checksum/cm,status --skip-crds --unified 6

# Diff a HelmRelease
fluxview diff hr podinfo --path clusters/prod/flux/
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
fluxview validate --path clusters/prod/flux/

# Specify schema directory
fluxview validate --path clusters/prod/flux/ --schema-dir /crds
```

Two schema formats are supported:
- **JSON Schema** (`.json`) — kubeconform-compatible (e.g., `crd-schemas.tar.gz` from flux2 releases)
- **CRD YAML** (`.yaml`/`.yml`) — standard Kubernetes CustomResourceDefinition manifests

Missing schemas never break the pipeline — resources without a matching schema are silently skipped.

## Flags

| Flag | Commands | Description |
|------|----------|-------------|
| `-p, --path` | build, diff, validate | Path to cluster directory with Kustomization files |
| `-n, --namespace` | build, diff, validate | Filter resources by namespace (default: all) |
| `--branch-orig` | diff | Branch/revision to compare against (default: auto-detect) |
| `--color` | diff | Color mode: `auto`, `always`, `never` |
| `--unified` | diff | Context lines (default: 3) |
| `--skip-crds` | build, diff | Skip CustomResourceDefinition resources |
| `--strip-attrs` | build, diff | Comma-separated keys to strip (e.g. `helm.sh/chart,status`) |
| `--schema-dir` | validate | Schema files directory |

## Exit codes

| Code | Meaning |
|------|---------|
| 0 | Success / no differences / all resources valid |
| 1 | Differences found (diff only) |
| 2 | Error |
| 3 | Validation failed (validate only) |

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
    - fluxview diff ks --path clusters/prod/flux/ --branch-orig master
        --strip-attrs helm.sh/chart,checksum/cm,status --skip-crds --color never
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

## TODO

- **Security: Docker image runs as root** — add non-root user to the runtime stage. Requires fixed UID/GID and `--user` documentation for bind-mount compatibility (see #4 in code review).
