# fluxview

CLI tool for building, diffing, and validating Flux GitOps resources locally. Works with a local git repository — no cluster connection required. All tools (git, kustomize, helm) are embedded via Go SDK; no external binaries needed.

## Quick start

```bash
go install github.com/banschikovde/fluxview/cmd/fluxview@latest
```

or via Docker:

```bash
docker run --rm -v $(pwd):/repo -w /repo ghcr.io/banschikovde/fluxview:latest \
  diff ks --path clusters/prod/flux/ --branch-orig master
```

## Features

- **build** — assemble Kustomization and HelmRelease resources
- **diff** — per-resource comparison against a git revision
- **validate** — validate resources against CRD schemas (Flux CRDs + any custom)
- Recursive Kustomization discovery following `spec.path` into shared bases (Flux controller behavior)
- postBuild variable substitution from ConfigMaps and Secrets (Secret values redacted with a placeholder)
- On-disk caching of Helm charts, remote resources, and kustomize builds — warm runs make no network requests and skip kustomize entirely ([details](docs/caching.md))
- Automatic secret redaction
- Box-header output format (per-resource, sorted by kind/namespace/name)

## Commands

Resource types accept the full name as well as the short alias: `kustomization` = `ks`, `helmrelease` = `hr`.

### build — assemble resources

Both `build ks` and `build hr` require Flux Kustomization files in `--path` (same contract).

```bash
# Build all Kustomizations (kustomize output: Flux CRs, HelmRelease, OCIRepository, etc.)
fluxview build ks --path clusters/prod/flux/

# Filter by namespace
fluxview build ks --path clusters/prod/flux/ --namespace cert-manager

# Without CRDs and noisy metadata
fluxview build ks --path clusters/prod/flux/ --skip-crds --strip-attrs status,creationTimestamp

# Inflate all HelmReleases (renders Helm chart templates) / a specific one
fluxview build hr --path clusters/prod/flux/
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
# Diff all Kustomizations / HelmReleases against master
fluxview diff ks --path clusters/prod/flux/ --branch-orig master
fluxview diff hr --path clusters/prod/flux/ --branch-orig master

# Only resources in flux-system namespace
fluxview diff ks --path clusters/prod/flux/ --branch-orig master --namespace flux-system

# Tuning: strip noisy attrs, skip CRDs, wider diff context
fluxview diff ks --path clusters/prod/flux/ --branch-orig master \
  --strip-attrs helm.sh/chart,checksum/cm,status --skip-crds --unified 6
```

Diff output is per-resource — each changed resource gets its own header followed by a line-level diff. In a TTY, changes are color-coded: green for added, red for removed. In pipes/CI, `+`/`-` prefixes are used.

`diff hr` is strict: if a HelmRelease cannot be built on either side (chart download failure, unresolvable chart source), the diff fails with exit code 2 instead of producing a partial comparison — a HelmRelease silently missing from one side would otherwise show up as a false "added"/"removed" diff. Deterministic skips (suspended releases, unsupported Bucket sources) stay warnings since they affect both sides equally.

### validate — validate resources

```bash
# Validate against CRD schemas (defaults to /crds/ or ./crds/)
fluxview validate --path clusters/prod/flux/

# Specify schema directory
fluxview validate --path clusters/prod/flux/ --schema-dir /crds
```

Two schema formats are supported: **JSON Schema** (`.json`, kubeconform-compatible — e.g. `crd-schemas.tar.gz` from flux2 releases) and **CRD YAML** (`.yaml`/`.yml`). Missing schemas never break the pipeline — resources without a matching schema are silently skipped.

## Flags

| Flag | Commands | Description |
|------|----------|-------------|
| `-p, --path` | build, diff, validate | Path to cluster directory with Kustomization files |
| `-n, --namespace` | build, diff, validate | Filter output resources by namespace (default: all) |
| `--branch-orig` | diff | Branch/revision to compare against (default: auto-detect) |
| `--color` | diff | Color mode: `auto`, `always`, `never` |
| `--unified` | diff | Context lines (default: 3) |
| `--skip-crds` | build, diff | Skip CustomResourceDefinition resources |
| `--strip-attrs` | build, diff | Comma-separated keys to strip (e.g. `helm.sh/chart,status`) |
| `--schema-dir` | validate | Schema files directory |

Cache flags (`--helm-*`, `--remote-*`, `--build-*`) share one pattern: `--<name>-cache-dir` (`off`/`none` disables) and `--<name>-cache-ttl` (`0` always bypasses) — see [docs/caching.md](docs/caching.md).

## Exit codes

| Code | Meaning |
|------|---------|
| 0 | Success / no differences / all resources valid |
| 1 | Differences found (diff only) |
| 2 | Error |
| 3 | Validation failed (validate only) |

## Caching

Three independent on-disk caches — Helm charts, remote kustomize resources, kustomize build outputs — all under `~/.cache/fluxview/` by default, so a single volume mount covers them. Both sides of a `diff` share one warm copy of everything. Defaults: indexes and floating remote refs re-check every `10m`, build outputs valid for `24h`.

Details, caveats, and per-cache flags/env vars: [docs/caching.md](docs/caching.md).

## Docker

```bash
docker run --rm -v $(pwd):/repo -w /repo ghcr.io/banschikovde/fluxview:latest \
  build ks --path clusters/prod/flux/
```

The image runs as a fixed non-root user; caches live inside the container unless mounted. For running as your host user, read-only repo mounts, and CRD schemas: [docs/docker.md](docs/docker.md).

## CRD schemas

Download Flux CRD schemas:

```bash
wget -qO- "https://github.com/fluxcd/flux2/releases/download/v2.9.1/crd-schemas.tar.gz" | tar xzf - -C ./crds
```

For custom CRDs (VictoriaMetrics, Kyverno, etc.), place YAML files alongside.

## CI

Example GitLab CI:

```yaml
fluxview:diff:
  image: ghcr.io/banschikovde/fluxview:latest
  variables:
    GIT_DEPTH: 0
  before_script:
    - git config --global --add safe.directory "${CI_PROJECT_DIR}"
    - git fetch origin ${CI_DEFAULT_BRANCH}
  script:
    - fluxview diff ks --path clusters/prod/flux/ --branch-orig origin/${CI_DEFAULT_BRANCH}
        --strip-attrs helm.sh/chart,checksum/cm,status --skip-crds --color never
  rules:
    - if: $CI_PIPELINE_SOURCE == "merge_request_event"
```

To keep downloads between jobs, cache the cache directories — see [docs/caching.md](docs/caching.md#caching-between-ci-jobs).

## Technology

- Go, Cobra CLI
- go-git (Git SDK)
- kustomize SDK (build)
- Helm SDK (inflation)
- k8s.io/apiextensions-apiserver (CRD validation)

## Limitations

These are intentional non-goals (not planned unless requested). In `build` the affected HelmReleases are skipped with a warning rather than rendered incorrectly; in `diff` they are skipped on both sides (warning), which never fails the strict check.

- **Bucket-sourced Helm charts are not supported** — `HelmRelease.spec.chart.spec.sourceRef.kind: Bucket` is not resolved (unlike `GitRepository`, which works since the chart already lives in the local checkout). Bucket content lives in S3-compatible object storage and would require fetching it separately (endpoint/credentials from `spec.secretRef`) — out of scope for now.
- **`HelmRelease.spec.chartRef.kind: HelmChart` is not supported** — only `chartRef.kind: OCIRepository` is resolved. Referencing a standalone `HelmChart` resource (used to share one chart artifact across multiple HelmReleases) is a rare, advanced pattern — HelmReleases using it are skipped with a warning ("has no chart name").

## License

Apache-2.0
