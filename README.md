# fluxview

CLI tool for building, diffing, and validating Flux GitOps resources locally. Works with a local git repository — no cluster connection required. All tools (git, kustomize, helm) are embedded via Go SDK; no external binaries needed.

## Features

- **build** — assemble Kustomization and HelmRelease resources
- **diff** — per-resource comparison against a git revision
- **validate** — validate resources against CRD schemas (Flux CRDs + any custom)
- Recursive Kustomization discovery following `spec.path` into shared bases (Flux controller behavior)
- Source resolution with repoRoot fallback for HelmRepository/OCIRepository outside `--path`
- postBuild variable substitution from ConfigMaps and Secrets (Secret values redacted with a placeholder)
- Helm chart caching — repo indexes and chart tarballs are cached on disk; repeated runs (and both sides of `diff hr`) don't re-download
- Kustomize remote resource caching — http(s) resources in kustomizations (CRD bundles, release assets) are cached on disk; warm `build ks`/`diff ks` runs make no network requests
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

The image runs as a fixed non-root user (65532:65532); the caches (Helm charts, kustomize remote resources) default to `/home/fluxview/.cache/fluxview/` inside the container and disappear with it — mount or point them elsewhere to keep them between runs:

```bash
docker run --rm -v $(pwd):/repo -v fluxview-cache:/home/fluxview/.cache/fluxview \
  -w /repo ghcr.io/banschikovde/fluxview:latest build hr --path clusters/prod/flux/
```

To run as your host user (e.g. so a mounted cache is owned correctly), override `--user` and point the caches to a writable path — an arbitrary UID has no writable HOME in the image:

```bash
docker run --rm --user "$(id -u):$(id -g)" \
  -v $(pwd):/repo -e FLUXVIEW_HELM_CACHE_DIR=/tmp/helm-cache -e FLUXVIEW_KUSTOMIZE_CACHE_DIR=/tmp/ks-cache \
  -w /repo ghcr.io/banschikovde/fluxview:latest build hr --path clusters/prod/flux/
```

The repository mount can be read-only (`:ro`): git worktrees for `diff` are written to `/tmp`.

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

Resource type accepts the full name as well as the short alias: `kustomization` = `ks`, `helmrelease` = `hr`.

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

Resource types: `ks`/`kustomization`, `hr`/`helmrelease` — same aliases as `build`.

```bash
# Diff all Kustomizations against master
fluxview diff ks --path clusters/prod/flux/ --branch-orig master

# Diff all HelmReleases against master
fluxview diff hr --path clusters/prod/flux/ --branch-orig master

# Diff only resources in flux-system namespace
fluxview diff ks --path clusters/prod/flux/ --branch-orig master --namespace flux-system

# Tuning: strip noisy attrs, skip CRDs, wider diff context
fluxview diff ks --path clusters/prod/flux/ --branch-orig master \
  --strip-attrs helm.sh/chart,checksum/cm,status --skip-crds --unified 6

# Diff a HelmRelease
fluxview diff hr podinfo --path clusters/prod/flux/
```

Diff output is per-resource — each changed resource gets its own header followed by a line-level diff. In a TTY, changes are color-coded: green for added, red for removed. In pipes/CI, `+`/`-` prefixes are used.

`diff hr` is strict: if a HelmRelease cannot be built on either side (chart download failure, unresolvable chart source), the diff fails with exit code 2 instead of producing a partial comparison — a HelmRelease silently missing from one side would otherwise show up as a false "added"/"removed" diff. Deterministic skips (suspended releases, unsupported Bucket sources) stay warnings since they affect both sides equally.

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
| `--helm-cache-dir` | build, diff | Helm cache directory for repo indexes and downloaded charts (default: `$FLUXVIEW_HELM_CACHE_DIR`, else `~/.cache/fluxview/helm`) |
| `--helm-index-ttl` | build, diff | How long cached Helm repo indexes and OCI tag resolutions stay fresh; `0` always refreshes (default: `10m`, env: `FLUXVIEW_HELM_INDEX_TTL`) |
| `--kustomize-cache-dir` | build, diff, validate | Cache directory for remote resources referenced by kustomizations (default: `$FLUXVIEW_KUSTOMIZE_CACHE_DIR`, else `~/.cache/fluxview/kustomize-remote`) |
| `--kustomize-cache-ttl` | build, diff, validate | How long cached remote kustomize resources with floating refs (branch/HEAD URLs) stay fresh; pinned version URLs never expire; `0` always refreshes (default: `10m`, env: `FLUXVIEW_KUSTOMIZE_CACHE_TTL`) |
| `--schema-dir` | validate | Schema files directory |

## Exit codes

| Code | Meaning |
|------|---------|
| 0 | Success / no differences / all resources valid |
| 1 | Differences found (diff only) |
| 2 | Error |
| 3 | Validation failed (validate only) |

## Caching

fluxview keeps two independent on-disk caches, each with its own directory, TTL and env vars: the **Helm chart cache** (repo indexes, chart tarballs) and the **kustomize remote resource cache** (remote resources referenced by kustomizations). They share nothing but a parent: both default under `~/.cache/fluxview/`, so a single volume mount covers both.

### Helm chart cache

Downloaded Helm charts and repository indexes are cached on disk, so repeated `build hr`/`diff hr` runs (and the two sides of every diff) resolve pinned chart versions without re-downloading.

- **Location**: `~/.cache/fluxview/helm` (or `$FLUXVIEW_HELM_CACHE_DIR`, or `$XDG_CACHE_HOME/fluxview/helm`; override per-run with `--helm-cache-dir`).
- **Charts**: tarballs are stored keyed by the sha256 digest from the repo index (or the OCI manifest digest) — a pinned chart version downloads once, then renders offline.
- **OCI**: tags are resolved to digests once per TTL (`oci-digests.yaml`); chart blobs come from the content cache by digest, so warm runs make zero registry requests. Floating versions (empty `spec.chart.spec.version` or a semver range, `OCIRepository.spec.ref.semver`) are resolved against the tag list, also cached with TTL (`oci-tags.yaml`). Digest-pinned refs (`spec.ref.digest`) are immutable and never re-resolve.
- **Indexes**: repo `index.yaml` is re-fetched when older than `--helm-index-ttl` (default `10m`). `--helm-index-ttl=0` always fetches a fresh index — use it if a newly published chart version is reported missing from a cached index.
- **Offline fallback**: if the index cannot be refreshed but a cached copy exists, the stale copy is used with a warning.
- **Credentials**: auth resolved from cluster Secrets is used for downloads but never written to the cache (`repositories.yaml` holds URLs only).
- **Cleanup**: the cache has no eviction — clear it any time with `rm -rf ~/.cache/fluxview/helm` (or your `--helm-cache-dir`); everything is re-downloadable.

Caveat: with a non-zero TTL, a floating chart version (empty or range `spec.chart.spec.version`, or a moved OCI tag) resolves against the cached index/digest — up to TTL stale. Pinned versions are unaffected (chart versions are immutable).

### Kustomize remote resource cache

Remote resources referenced by kustomizations (http(s) entries in `resources:`, e.g. CRD bundles from `raw.githubusercontent.com` or GitHub release assets) are cached on disk, so `build ks`/`diff ks` (and the HelmRelease pipeline) make no network requests for them on warm runs.

- **Location**: `~/.cache/fluxview/kustomize-remote` (or `$FLUXVIEW_KUSTOMIZE_CACHE_DIR`, or `$XDG_CACHE_HOME/fluxview/kustomize-remote`; override per-run with `--kustomize-cache-dir`). Files are stored as `<sha256(url)>.yaml` with a `.url` sidecar naming the source.
- **How it works**: before the first build the repository is scanned for remote references and every file-like URL is downloaded once with a plain HTTP GET (GitHub release assets are fetched the same way — no git clone). Kustomization files are then served to kustomize with URLs rewritten to absolute cache paths, so kustomize itself never fetches anything and its output is byte-identical to a non-cached build.
- **Pinned vs floating**: URLs with a version marker (`releases/download/vX.Y.Z/…`, `raw/…/<tag>/…`, `?ref=<tag|sha>`) are immutable — downloaded once, served forever, no TTL. Everything else (branch/HEAD refs like `main`, unversioned URLs) honors `--kustomize-cache-ttl`; an expired entry is re-fetched, and if the refresh fails the stale copy is used with a warning.
- **What is not cached**: git/directory bases (`github.com/org/repo/path?ref=…` as a kustomization base, remote `components:`) are left for kustomize itself and warned about once — they still trigger kustomize's own git fetch. A URL that cannot be downloaded and has no cached copy is likewise left untouched, preserving the pre-cache behavior (warning + skip in `build`, strict failure in `diff`). Private/authenticated URLs are out of scope.
- **Scope**: only `resources:` entries are cached. Remote URLs in `crds:`, `patches:`, `configurations:` and `transformers:` (if any) are not scanned or rewritten — kustomize fetches them itself on every build, as before.
- **Integrity**: cached entries are validated as YAML on every use — a corrupted entry is re-downloaded instead of failing the build.

Caveat: with `--kustomize-cache-ttl=0` every `diff` side re-fetches floating URLs independently, so a remote resource that changes mid-run can surface as a phantom diff unrelated to the commit. With a non-zero TTL (default) both diff sides share one fresh cached copy, so this cannot happen.

In CI, mount or cache the directories to keep downloads between jobs (GitLab example):

```yaml
fluxview:diff:
  image: ghcr.io/banschikovde/fluxview:latest
  cache:
    key: fluxview-cache
    paths:
      - .cache/
  variables:
    FLUXVIEW_HELM_CACHE_DIR: .cache/helm
    FLUXVIEW_KUSTOMIZE_CACHE_DIR: .cache/kustomize-remote
  script:
    - fluxview diff hr --path clusters/prod/flux/ --branch-orig master
        --strip-attrs helm.sh/chart,checksum/cm,status --skip-crds --color never
  rules:
    - if: $CI_MERGE_REQUEST_ID
```

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

## Limitations

These are intentional non-goals (not planned unless requested). In `build` the affected HelmReleases are skipped with a warning rather than rendered incorrectly; in `diff` they are skipped on both sides (warning), which never fails the strict check.

- **Bucket-sourced Helm charts are not supported** — `HelmRelease.spec.chart.spec.sourceRef.kind: Bucket` is not resolved (unlike `GitRepository`, which works since the chart already lives in the local checkout). Bucket content lives in S3-compatible object storage and would require fetching it separately (endpoint/credentials from `spec.secretRef`) — out of scope for now.
- **`HelmRelease.spec.chartRef.kind: HelmChart` is not supported** — only `chartRef.kind: OCIRepository` is resolved. Referencing a standalone `HelmChart` resource (used to share one chart artifact across multiple HelmReleases) is a rare, advanced pattern — HelmReleases using it are skipped with a warning ("has no chart name").
