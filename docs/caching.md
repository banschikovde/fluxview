# Caching

fluxview keeps six independent on-disk caches, one per kind of expensive work: the **Helm chart cache** (repo indexes, chart tarballs), the **remote resource cache** (files fetched by URL and referenced by kustomizations), the **build cache** (kustomize build outputs), the **git source cache** (clones of external GitRepository sources), the **schema cache** (validation schemas downloaded over HTTP) and the **CRD schema cache** (schemas converted from CRD YAML manifests). The first four follow the same conventions — flags `--<name>-cache-dir`/`--<name>-cache-ttl`, dir `off`/`none` disables, ttl `0` always bypasses (still writes); the Helm, build and git source caches also honor `FLUXVIEW_HELM_*`/`FLUXVIEW_BUILD_CACHE_*`/`FLUXVIEW_GIT_SOURCE_*` env vars, the remote cache honors `FLUXVIEW_REMOTE_CACHE_TIMEOUT` (its download timeout) only. The schema caches have no flags — they are always on (entries are version-pinned or keyed by source mtime, there is nothing to configure). They share nothing but a parent: all default under `~/.cache/fluxview/`, so a single volume mount covers them.

## Cache flags

| Flag | Commands | Description |
|------|----------|-------------|
| `--helm-cache-dir` | build, diff | Helm cache directory for repo indexes and downloaded charts (default: `$FLUXVIEW_HELM_CACHE_DIR`, else `~/.cache/fluxview/helm`) |
| `--helm-index-ttl` | build, diff | How long cached Helm repo indexes and OCI tag resolutions stay fresh (default: `10m`, env: `FLUXVIEW_HELM_INDEX_TTL`) |
| `--helm-download-timeout` | build, diff | Per-request timeout for downloading Helm repo indexes and chart tarballs; `0` = no limit (default: `2m`, env: `FLUXVIEW_HELM_DOWNLOAD_TIMEOUT`) |
| `--remote-cache-dir` | build, diff, validate | Cache directory for remote resources referenced by kustomizations (default: `~/.cache/fluxview/kustomize-remote`) |
| `--remote-cache-ttl` | build, diff, validate | How long cached remote resources with floating refs (branch/HEAD URLs) stay fresh; pinned version URLs never expire (default: `10m`) |
| `--remote-cache-timeout` | build, diff, validate | Per-request timeout for downloading remote resources referenced by kustomizations; `0` = no limit (default: `30s`, env: `FLUXVIEW_REMOTE_CACHE_TIMEOUT`) |
| `--build-cache-dir` | build, diff, validate | Cache directory for kustomize build outputs, reused while input files are unchanged (default: `$FLUXVIEW_BUILD_CACHE_DIR`, else `~/.cache/fluxview/kustomize-builds`) |
| `--build-cache-ttl` | build, diff, validate | How long cached build outputs stay usable (default: `24h`, env: `FLUXVIEW_BUILD_CACHE_TTL`) |
| `--git-source-cache-dir` | build, diff, validate | Cache directory for clones of external GitRepository sources (default: `$FLUXVIEW_GIT_SOURCE_CACHE_DIR`, else `~/.cache/fluxview/git-sources`) |
| `--git-source-cache-ttl` | build, diff, validate | How long floating external source resolutions (branch/semver/HEAD) stay fresh; pinned commit/tag clones never expire (default: `10m`, env: `FLUXVIEW_GIT_SOURCE_CACHE_TTL`) |

Conventions shared by all caches:

- a `-dir` value of `off`/`none`/`disabled` turns that cache off completely;
- a `-ttl` of `0` always bypasses it — remote refs are re-fetched, indexes re-read, builds re-run — but fresh results are still written to disk for later runs;
- the download-timeout flags (`--helm-download-timeout`, `--remote-cache-timeout`) treat a negative value like `0` (no limit) with a warning — the same forgiving normalization as the `-ttl` flags. This is deliberately different from validate's `--schema-download-timeout`, which rejects negative values outright (exit 2).

## Helm chart cache

Downloaded Helm charts and repository indexes are cached on disk, so repeated `build hr`/`diff hr` runs (and the two sides of every diff) resolve pinned chart versions without re-downloading.

- **Location**: `~/.cache/fluxview/helm` (or `$FLUXVIEW_HELM_CACHE_DIR`, or `$XDG_CACHE_HOME/fluxview/helm`; override per-run with `--helm-cache-dir`).
- **Charts**: tarballs are stored keyed by the sha256 digest from the repo index (or the OCI manifest digest) — a pinned chart version downloads once, then renders offline.
- **OCI**: tags are resolved to digests once per TTL (`oci-digests.yaml`); chart blobs come from the content cache by digest, so warm runs make zero registry requests. Floating versions (empty `spec.chart.spec.version` or a semver range, `OCIRepository.spec.ref.semver`) are resolved against the tag list, also cached with TTL (`oci-tags.yaml`). Digest-pinned refs (`spec.ref.digest`) are immutable and never re-resolve.
- **Indexes**: repo `index.yaml` is re-fetched when older than `--helm-index-ttl` (default `10m`). `--helm-index-ttl=0` always fetches a fresh index — use it if a newly published chart version is reported missing from a cached index.
- **Slow networks**: index and tarball downloads are bounded by `--helm-download-timeout` (default `2m`, the Helm SDK's own default). On a slow link raise it (`--helm-download-timeout=30m`) or disable it entirely (`--helm-download-timeout=0`). The flag applies to HTTP(S) repositories only — OCI behavior is deliberately different, see the caveat below.
- **Offline fallback**: if the index cannot be refreshed but a cached copy exists, the stale copy is used with a warning.
- **Credentials**: auth resolved from cluster Secrets is used for downloads but never written to the cache (`repositories.yaml` holds URLs only).
- **Cleanup**: the cache has no eviction — clear it any time with `rm -rf ~/.cache/fluxview/helm` (or your `--helm-cache-dir`); everything is re-downloadable.

Caveat: with a non-zero TTL, a floating chart version (empty or range `spec.chart.spec.version`, or a moved OCI tag) resolves against the cached index/digest — up to TTL stale. Pinned versions are unaffected (chart versions are immutable).

Caveat (OCI has no download timeout): OCI operations (digest/tag resolution, chart pulls) are deliberately not bounded by `--helm-download-timeout` — they wait as long as the network takes, which is what very slow links need. The trade-off: an unreachable registry fails on its own within roughly a minute (TCP dial timeout plus the registry client's built-in retries), but a registry that accepts the connection and then hangs — or trickles bytes forever — is waited on indefinitely; the command will not terminate by itself. `Ctrl-C` (or `SIGTERM`) always aborts immediately: every OCI operation is wrapped in cancellation even though the underlying SDK calls accept no context.

## Remote resource cache

Remote resources referenced by kustomizations (http(s) entries in `resources:`, e.g. CRD bundles from `raw.githubusercontent.com` or GitHub release assets) are cached on disk, so `build ks`/`diff ks` (and the HelmRelease pipeline) make no network requests for them on warm runs.

- **Location**: `~/.cache/fluxview/kustomize-remote` (or `$XDG_CACHE_HOME/fluxview/kustomize-remote`; override per-run with `--remote-cache-dir`, disable with `--remote-cache-dir=off`). Files are stored as `<sha256(url)>.yaml` with a `.url` sidecar naming the source.
- **How it works**: before the first build the repository is scanned for remote references and every file-like URL is downloaded once with a plain HTTP GET (GitHub release assets are fetched the same way — no git clone). Kustomization files are then served to kustomize with URLs rewritten to absolute cache paths, so kustomize itself never fetches anything and its output is byte-identical to a non-cached build.
- **Pinned vs floating**: URLs with a version marker (`releases/download/vX.Y.Z/…`, `raw/…/<tag>/…`, `?ref=<tag|sha>`) are immutable — downloaded once, served forever, no TTL. Everything else (branch/HEAD refs like `main`, unversioned URLs) honors `--remote-cache-ttl`; an expired entry is re-fetched, and if the refresh fails the stale copy is used with a warning.
- **What is not cached**: git/directory bases (`github.com/org/repo/path?ref=…` as a kustomization base, remote `components:`) are left for kustomize itself and warned about once — they still trigger kustomize's own git fetch. A URL that cannot be downloaded and has no cached copy is likewise left untouched, preserving the pre-cache behavior (warning + skip in `build`, strict failure in `diff`). Private/authenticated URLs are out of scope.
- **Scope**: only `resources:` entries are cached. Remote URLs in `crds:`, `patches:`, `configurations:` and `transformers:` (if any) are not scanned or rewritten — kustomize fetches them itself on every build, as before.
- **Integrity**: cached entries are validated as YAML on every use — a corrupted entry is re-downloaded instead of failing the build.
- **Slow networks**: each download is bounded by `--remote-cache-timeout` (default `30s`). On a slow link raise it or disable it entirely (`--remote-cache-timeout=0`); a URL that still cannot be downloaded falls back to kustomize's own fetch, as before.

Caveat: with `--remote-cache-ttl=0` every `diff` side re-fetches floating URLs independently, so a remote resource that changes mid-run can surface as a phantom diff unrelated to the commit. With a non-zero TTL (default) both diff sides share one fresh cached copy, so this cannot happen.

## Kustomize build cache

Kustomize builds dominate fluxview's CPU cost (the SDK re-parses and re-serializes every input file on each run). The build cache stores the final output of every kustomize build together with an input manifest — the exact files read during the build (path, sha256 of content) plus a listing of every directory involved (sorted entry names). A cached output is served only when re-hashing every recorded file and re-listing every recorded directory reproduces the manifest exactly. Identity is **content**, not mtime, so entries survive fresh git checkouts — which is what makes the cache useful across CI jobs: a pipeline rebuilds only the subtrees its commit actually touched.

- **Location**: `~/.cache/fluxview/kustomize-builds` (or `$FLUXVIEW_BUILD_CACHE_DIR`, or `$XDG_CACHE_HOME/fluxview/kustomize-builds`; override per-run with `--build-cache-dir`; `off`/`none` disables).
- **Invalidation is automatic**: any content change in a recorded file, or any file added/removed/renamed in a recorded directory, forces a rebuild of exactly the affected directories. Touching files without changing content (fresh checkouts, branch switches of identical trees) keeps entries valid.
- **Effect**: warm runs skip kustomize entirely — roughly half the CPU of a cold run; schema loading and post-processing still apply. In CI with a shared cache directory, unchanged subtrees (shared bases, unrelated apps) are served from the previous job's entries. First (cold) runs are unaffected; the lookup overhead (re-hashing the inputs kustomize would read anyway) is milliseconds.
- **TTL**: entries older than `--build-cache-ttl` (default `24h`) are rebuilt once; `--build-cache-ttl=0` always rebuilds (fresh outputs are still cached for later runs). With content addressing this is a belt-and-braces bound, not a correctness need.
- **Remote resources**: refreshed remote-cache files change content, which invalidates dependent build entries on the next run.
- **Versioning**: entries are salted with the fluxview and kustomize library versions; after an upgrade the cache repopulates automatically.
- **Cleanup**: oldest entries are evicted beyond 2048 entries / 256 MB; clear any time with `rm -rf ~/.cache/fluxview/kustomize-builds` — everything is rebuildable.

Caveats: keep the cache directory **outside** directories containing kustomizations — a cache entry appearing inside a recorded directory changes its listing and needlessly invalidates entries (the CI examples put `.cache/` at the repository root, away from the cluster trees). Floating remote resources are refreshed before the first build-cache lookup of each run (each fluxview invocation is a fresh process in CI), so a changed remote copy invalidates dependent entries immediately rather than waiting out the build TTL; pinned URLs never change and never invalidate.

## Git source cache

Flux Kustomizations whose `sourceRef` names a GitRepository of a **different** repository than the local origin (the classic dedicated-CRDs-upstream pattern, e.g. Kyverno) have their `spec.path` in that upstream, not in the cluster checkout. fluxview fetches such upstreams into the git source cache and builds from the clone, so `build`/`diff`/`validate` see the external resources exactly like local ones.

- **Location**: `~/.cache/fluxview/git-sources` (or `$FLUXVIEW_GIT_SOURCE_CACHE_DIR`, or `$XDG_CACHE_HOME/fluxview/git-sources`; override per-run with `--git-source-cache-dir`).
- **Layout**: content-addressed and immutable — each clone lives in `data/<sha256(url + resolved-ref)>/` with a `.fluxview-source.json` seed; a directory never changes once written. Directories and the small floating-ref pointer files in `refs/` appear atomically (temp + rename), so concurrent fluxview processes sharing the cache never see a partial clone.
- **Pinned vs floating**: `ref.commit` and `ref.tag` map straight to their clone — fetched once, reused forever, no TTL. A `ref.commit` clone is always a full clone followed by a checkout of the SHA (shallow fetch of an arbitrary SHA is not guaranteed by servers); tags clone shallowly over http(s)/git. `ref.branch`, `ref.semver` and a missing ref (HEAD) are floating: the resolution (branch → commit sha via ls-remote, semver → highest matching tag) is cached in a pointer file and re-done once older than `--git-source-cache-ttl` (default `10m`); `0` re-resolves every run. A moved branch or a new semver winner resolves to a different immutable clone — the old one simply stays. When a remote advertises a zero-hash symbolic HEAD (some servers/transports) the default branch is derived from the single branch, falling back to the conventional `main`/`master` names before failing.
- **Scope**: public repositories over http(s)/git/file; ssh URLs needing credentials are not fetched (no auth). `spec.secretRef`, `spec.proxySecretRef` and `spec.verify` are ignored; `spec.ignore` is not applied (it shapes the source-controller artifact, not the result of building `spec.path` against a full clone). OCIRepository/Bucket sources are not fetched. Path resolution is source-first like Flux: an external GitRepository means `spec.path` always comes from the upstream clone, even when a same-name directory exists locally (a fetch failure then warns and skips in build/diff, and fails validate — the local copy is a different repository's content and is never a fallback).
- **`off` disables reuse, not fetching**: `--git-source-cache-dir=off` still builds external sources — every run clones into a private temp directory, all removed when the command exits. A repository whose external source cannot be fetched warns and skips in `build`/`diff`, and fails `validate` (exit 2).
- **Timeouts**: per-request git timeouts are not configured (the SDK accepts a context but has no built-in per-request limit) — a dead upstream eventually fails on its own transport errors, and `Ctrl-C`/`SIGTERM` always aborts immediately through the wired context. Cold clones of large upstreams (tens of MB) can take a while on slow links; afterwards pinned clones make zero network requests.
- **Offline CI**: a mounted warm cache plus pinned refs (`ref.tag`/`ref.commit`) is a fully deterministic offline run — pinned clones are immutable and never touch the network (verifiable by pointing `HTTPS_PROXY` at a dead port). Floating refs (branch/semver/HEAD) re-resolve once the TTL expires and will fail offline by design; extend the TTL or pin the refs for isolated runners. Note: `validate` additionally needs schemas — point `--schema-dir` at a kubernetes-json-schema checkout to keep it fully offline (the default schema registry requires network on a cold schema cache).
- **Diff sides**: both sides of a diff share one cache, so a pinned external source is byte-identical across the comparison. With a zero TTL a floating ref re-resolves per side and can surface a phantom diff — the same caveat as the remote cache.
- **Cleanup**: no eviction — clear any time with `rm -rf ~/.cache/fluxview/git-sources`; everything is re-clonable.

## Schema cache

`validate` downloads the Kubernetes schemas it needs from the kubeconform default registry before validating: a bounded, parallel prefetch (per-request timeout, retries, interruptible) fills the on-disk cache, and validation itself reads only from disk — after one run per Kubernetes version, `validate` works offline for built-in kinds. A registry failure worse than a missing schema fails the run (exit 2) rather than hanging.

- **Location**: `~/.cache/fluxview/schemas` (or `$XDG_CACHE_HOME/fluxview/schemas`); the prefetched registry copy sits in the `registry/` subdirectory in the kubernetes-json-schema layout.
- **No TTL, no flags**: entries are keyed by the exact schema URL, which includes the pinned `--kubernetes-version` — they never change and never expire. Bump `--kubernetes-version` and the new version simply populates the cache alongside the old one.
- **Offline**: schemas from `--schema-dir` (local JSON/CRD files) never touch the network; a cold cache without network fails the run up front instead of hanging mid-validation.
- **Cleanup**: the cache has no eviction — clear it any time with `rm -rf ~/.cache/fluxview/schemas`; everything is re-downloadable.

## CRD schema cache

CRD YAML manifests from `--schema-dir` are converted to kubeconform JSON schemas once and cached under `~/.cache/fluxview/crd-schemas` (one subdirectory per source directory, keyed by a path hash). A CRD file is reconverted only when its size or modification time changes; schemas of removed files are dropped automatically. `rm -rf ~/.cache/fluxview/crd-schemas` forces a full reconversion.

## Caching between CI jobs

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
    FLUXVIEW_BUILD_CACHE_DIR: .cache/kustomize-builds
    FLUXVIEW_GIT_SOURCE_CACHE_DIR: .cache/git-sources
  script:
    - fluxview diff hr --path clusters/prod/flux/ --branch-orig master
        --remote-cache-dir .cache/kustomize-remote
        --strip-attrs helm.sh/chart,checksum/cm,status --skip-crds --color never
  rules:
    - if: $CI_MERGE_REQUEST_ID
```
