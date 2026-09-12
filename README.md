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
- Kustomize build caching — build outputs are cached on disk while input files are unchanged; warm runs skip kustomize entirely (~2× less CPU)
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
  -v $(pwd):/repo -e FLUXVIEW_HELM_CACHE_DIR=/tmp/helm-cache \
  -w /repo ghcr.io/banschikovde/fluxview:latest \
  build hr --path clusters/prod/flux/ --remote-cache-dir /tmp/ks-cache
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
| `-n, --namespace` | build, diff, validate | Filter output resources by namespace (default: all) |
| `--branch-orig` | diff | Branch/revision to compare against (default: auto-detect) |
| `--color` | diff | Color mode: `auto`, `always`, `never` |
| `--unified` | diff | Context lines (default: 3) |
| `--skip-crds` | build, diff | Skip CustomResourceDefinition resources |
| `--strip-attrs` | build, diff | Comma-separated keys to strip (e.g. `helm.sh/chart,status`) |
| `--schema-dir` | validate | Schema files directory |

### Cache flags

The three caches (see [Caching](#caching)) share one flag pattern — `<name>` is what the cache holds: `helm`, `remote`, `build`.

| Flag | Commands | Description |
|------|----------|-------------|
| `--helm-cache-dir` | build, diff | Helm cache directory for repo indexes and downloaded charts (default: `$FLUXVIEW_HELM_CACHE_DIR`, else `~/.cache/fluxview/helm`) |
| `--helm-index-ttl` | build, diff | How long cached Helm repo indexes and OCI tag resolutions stay fresh (default: `10m`, env: `FLUXVIEW_HELM_INDEX_TTL`) |
| `--remote-cache-dir` | build, diff, validate | Cache directory for remote resources referenced by kustomizations (default: `~/.cache/fluxview/kustomize-remote`) |
| `--remote-cache-ttl` | build, diff, validate | How long cached remote resources with floating refs (branch/HEAD URLs) stay fresh; pinned version URLs never expire (default: `10m`) |
| `--build-cache-dir` | build, diff, validate | Cache directory for kustomize build outputs, reused while input files are unchanged (default: `$FLUXVIEW_BUILD_CACHE_DIR`, else `~/.cache/fluxview/kustomize-builds`) |
| `--build-cache-ttl` | build, diff, validate | How long cached build outputs stay usable (default: `24h`, env: `FLUXVIEW_BUILD_CACHE_TTL`) |

Conventions shared by all caches:

- a `-dir` value of `off`/`none`/`disabled` turns that cache off completely;
- a `-ttl` of `0` always bypasses it — remote refs are re-fetched, indexes re-read, builds re-run — but fresh results are still written to disk for later runs.

## Exit codes

| Code | Meaning |
|------|---------|
| 0 | Success / no differences / all resources valid |
| 1 | Differences found (diff only) |
| 2 | Error |
| 3 | Validation failed (validate only) |

## Caching

fluxview keeps three independent on-disk caches, one per kind of expensive work: the **Helm chart cache** (repo indexes, chart tarballs), the **remote resource cache** (files fetched by URL and referenced by kustomizations) and the **build cache** (kustomize build outputs). All follow the same conventions — flags `--<name>-cache-dir`/`--<name>-cache-ttl`, dir `off`/`none` disables, ttl `0` always bypasses (still writes). The Helm and build caches also honor `FLUXVIEW_HELM_*`/`FLUXVIEW_BUILD_CACHE_*` env vars; the remote cache is flags-only. They share nothing but a parent: all default under `~/.cache/fluxview/`, so a single volume mount covers them.

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

### Remote resource cache

Remote resources referenced by kustomizations (http(s) entries in `resources:`, e.g. CRD bundles from `raw.githubusercontent.com` or GitHub release assets) are cached on disk, so `build ks`/`diff ks` (and the HelmRelease pipeline) make no network requests for them on warm runs.

- **Location**: `~/.cache/fluxview/kustomize-remote` (or `$XDG_CACHE_HOME/fluxview/kustomize-remote`; override per-run with `--remote-cache-dir`, disable with `--remote-cache-dir=off`). Files are stored as `<sha256(url)>.yaml` with a `.url` sidecar naming the source.
- **How it works**: before the first build the repository is scanned for remote references and every file-like URL is downloaded once with a plain HTTP GET (GitHub release assets are fetched the same way — no git clone). Kustomization files are then served to kustomize with URLs rewritten to absolute cache paths, so kustomize itself never fetches anything and its output is byte-identical to a non-cached build.
- **Pinned vs floating**: URLs with a version marker (`releases/download/vX.Y.Z/…`, `raw/…/<tag>/…`, `?ref=<tag|sha>`) are immutable — downloaded once, served forever, no TTL. Everything else (branch/HEAD refs like `main`, unversioned URLs) honors `--remote-cache-ttl`; an expired entry is re-fetched, and if the refresh fails the stale copy is used with a warning.
- **What is not cached**: git/directory bases (`github.com/org/repo/path?ref=…` as a kustomization base, remote `components:`) are left for kustomize itself and warned about once — they still trigger kustomize's own git fetch. A URL that cannot be downloaded and has no cached copy is likewise left untouched, preserving the pre-cache behavior (warning + skip in `build`, strict failure in `diff`). Private/authenticated URLs are out of scope.
- **Scope**: only `resources:` entries are cached. Remote URLs in `crds:`, `patches:`, `configurations:` and `transformers:` (if any) are not scanned or rewritten — kustomize fetches them itself on every build, as before.
- **Integrity**: cached entries are validated as YAML on every use — a corrupted entry is re-downloaded instead of failing the build.

Caveat: with `--remote-cache-ttl=0` every `diff` side re-fetches floating URLs independently, so a remote resource that changes mid-run can surface as a phantom diff unrelated to the commit. With a non-zero TTL (default) both diff sides share one fresh cached copy, so this cannot happen.

### Kustomize build cache

Kustomize builds dominate fluxview's CPU cost (the SDK re-parses and re-serializes every input file on each run). The build cache stores the final output of every kustomize build together with an input manifest — the exact files read during the build (path, size, mtime) plus the directories visited (path, mtime). A cached output is served only while a fresh stat of every recorded input still matches exactly; otherwise the build runs normally and refreshes the entry.

- **Location**: `~/.cache/fluxview/kustomize-builds` (or `$FLUXVIEW_BUILD_CACHE_DIR`, or `$XDG_CACHE_HOME/fluxview/kustomize-builds`; override per-run with `--build-cache-dir`; `off`/`none` disables).
- **Invalidation is automatic**: any edit, `git checkout`, branch switch or file added/removed in a visited directory changes a recorded size/mtime and forces a rebuild of exactly the affected directories. Fresh git worktrees (diff comparison states) always have fresh mtimes, so they never hit stale entries.
- **Effect**: warm runs on an unchanged tree skip kustomize entirely — roughly half the CPU of a cold run; schema loading and post-processing still apply. First (cold) runs are unaffected.
- **TTL**: entries older than `--build-cache-ttl` (default `24h`) are rebuilt once; `--build-cache-ttl=0` always rebuilds (fresh outputs are still cached for later runs). The TTL bounds staleness for scenarios mtimes cannot express (e.g. content restored with preserved mtimes by `rsync -a`/`tar`).
- **Remote resources**: refreshed remote-cache files get new mtimes, which invalidate dependent build entries on the next run.
- **Versioning**: entries are salted with the fluxview and kustomize library versions; after an upgrade the cache repopulates automatically.
- **Cleanup**: oldest entries are evicted beyond 2048 entries / 256 MB; clear any time with `rm -rf ~/.cache/fluxview/kustomize-builds` — everything is rebuildable.

Caveat: like make/bazel mtime caching, the cache trusts that changed content means changed mtime. Normal editors and git always update mtimes, so this holds in practice; the TTL is the safety net.

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
  script:
    - fluxview diff hr --path clusters/prod/flux/ --branch-orig master
        --remote-cache-dir .cache/kustomize-remote --build-cache-dir .cache/kustomize-builds
        --strip-attrs helm.sh/chart,checksum/cm,status --skip-crds --color never
  rules:
    - if: $CI_MERGE_REQUEST_ID
```

## Resource usage

Measured on a mid-sized repository (45 kustomizations, ~550 YAML files): a cold `validate` costs ~2 CPU-seconds and peaks at ~190 MB RSS (the kustomize SDK's YAML object model allocates ~1 GB transiently); a warm run with the build cache costs ~0.85 CPU-seconds and peaks near ~120 MB. Memory is transient — after a run the process retains only a few MB — but the **peak** is what container limits must accommodate.

Suggested container settings:

```yaml
resources:
  requests: { cpu: 100m, memory: 64Mi }
  limits:   { cpu: "1",  memory: 256Mi }
```

Notes:

- CPU limits below 500m stretch every run proportionally (~2 CPU-seconds cold, ~1 warm); on a 250m limit a cold run takes ~8s of wall time.
- Do **not** set aggressive `GOMEMLIMIT` values: the peak is dominated by live working structures, not by unused heap headroom. Measured: `GOMEMLIMIT=96MiB` trims the cold peak by only ~15% for +35% CPU, and `64MiB` makes the GC thrash (~3.5× CPU). If you need a hard cap below 256Mi, test your workload first.
- Numbers are from macOS; Linux containers typically report slightly lower peaks (the runtime returns pages more eagerly).

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
