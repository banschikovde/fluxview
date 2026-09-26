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
- **validate** — schema validation via the kubeconform engine: native Kubernetes kinds out of the box, Flux/custom CRD schemas via `--schema-dir`; supports `--strict`, `--skip-kind`, `--output json|junit`
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
# Kubernetes resources are validated out of the box (schemas for
# --kubernetes-version, downloaded on first use and cached)
fluxview validate --path clusters/prod/flux/

# Add schemas for Flux CRDs and custom resources
fluxview validate --path clusters/prod/flux/ --schema-dir /schemas

# Pin a different Kubernetes schema version
fluxview validate --path clusters/prod/flux/ --kubernetes-version 1.34.0

# Strict mode: reject duplicated YAML keys; strict default-registry
# schemas also reject unknown fields
fluxview validate --path clusters/prod/flux/ --strict

# Full offline mode: unpack a kubernetes-json-schema checkout into the
# schema directory — native kinds validate locally, no network
mkdir -p schemas/kubernetes-json-schema
cp -r /path/to/kubernetes-json-schema/v1.36.1-standalone* schemas/kubernetes-json-schema/
```

Validation uses the [kubeconform](https://github.com/yannh/kubeconform) engine. Schema sources, in priority order (first match wins):

1. `--schema-dir` (default: `/schemas/` or `./schemas/`):
   - **JSON Schema** files (`.json`, kubeconform-compatible — e.g. `crd-schemas.tar.gz` from flux2 releases)
   - **CRD YAML** manifests (`.yaml`/`.yml`, converted on the fly)
2. A **kubernetes-json-schema checkout** found in or one level under `--schema-dir`: directories named `v<version>-standalone[-strict]` — the same layout the default registry serves. Grab the dirs for your `--kubernetes-version` from [yannh/kubernetes-json-schema](https://github.com/yannh/kubernetes-json-schema), `-standalone-strict` too if you use `--strict`.
3. The default registry of Kubernetes schemas for `--kubernetes-version`: before validation, the needed schemas are downloaded in parallel into the local cache (per-request timeout `--schema-download-timeout`, retries, interruptible) and validation then reads them from disk — it never waits on the network. A registry failure (other than a missing schema) fails the run (exit 2) instead of hanging or silently skipping.

Behavior:

- Resources without a matching schema are silently skipped; an unreadable `--schema-dir` — or one containing a malformed CRD YAML file or a schema that cannot be converted — fails the run, so typos and corrupt schema sources don't silently disable CRD validation.
- `--kubernetes-version` must be a full `X.Y.Z` version (or `master`); anything else fails fast (exit 2) — a short form like `1.36` would 404 every native-kind schema and look like a green run while validating nothing. If the registry has no schema for any of the fetched kinds (a published-but-wrong version looks exactly like that), validate warns about the mass skip.
- A failed kustomize build or a Kustomization whose `spec.path` is missing from the repository (suspended ones exempt) fails the run (exit 2) — a validation gate must not report success on a partial build.
- Kustomizations whose `sourceRef` names an **external** `GitRepository` (a repository other than the local origin, e.g. a dedicated CRDs upstream like `kyverno/kyverno`) are fetched into the [git source cache](docs/caching.md) and their resources are built, diffed and validated like local ones — including directories of loose YAML files without a `kustomization.yaml`. The path always resolves against the upstream clone (Flux source-first semantics), even when a same-name directory exists locally. A broken manifest in the upstream fails the gate; an unreachable upstream fails validate (exit 2) and warns-and-skips in build/diff. **Private upstreams authenticate from the machine environment**, like native git: an SSH key (`FLUXVIEW_GIT_SSH_KEY`, ssh-agent or `~/.ssh/id_*`) with strict known_hosts verification (`--git-source-ssh-known-hosts`, TOFU via `--git-source-ssh-accept-new`), or https basic auth (`FLUXVIEW_GIT_USERNAME`/`FLUXVIEW_GIT_PASSWORD`, `FLUXVIEW_GIT_TOKEN`, `~/.netrc`) — https credentials apply only to hosts listed in `FLUXVIEW_GIT_CREDENTIAL_HOSTS` (fail-closed when unset; a manifest URL is untrusted input) — see the [git source cache](docs/caching.md) section. OCIRepository/Bucket sources stay outside the checkout and keep the missing-path behavior.
- `--output json` / `--output junit` emit a machine-readable report to stdout for CI.

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
| `--schema-dir` | validate | Directory with schemas: kubeconform JSON files, CRD YAML manifests and/or a kubernetes-json-schema checkout (default: `/schemas/` or `./schemas/`) |
| `--kubernetes-version` | validate | Kubernetes version (full `X.Y.Z` like `1.34.0`, or `master`) for the default schema location (default: `1.36.1`) |
| `--schema-download-timeout` | validate | Per-request timeout for downloading Kubernetes schemas (e.g. `30s`, `1m`; `0` = no limit, Ctrl-C still interrupts; default: `30s`) |
| `--strict` | validate | Reject duplicated YAML keys; strict default-registry schemas also reject unknown fields |
| `--skip-kind` | validate | Kinds to skip (repeatable or comma-separated): `Deployment` (any apiVersion) or `apps/v1/Deployment` |
| `--output` | validate | Output format: `text` (default, stderr), `json` or `junit` (stdout, per-resource statuses + summary) |

Cache flags (`--helm-*`, `--remote-*`, `--build-*`, `--git-source-*`) share one pattern: `--<name>-cache-dir` (`off`/`none` disables) and `--<name>-cache-ttl` (`0` always bypasses). On slow networks, `--helm-download-timeout` and `--remote-cache-timeout` (`0` = no limit) let downloads wait as long as the link needs instead of being cut off after `2m`/`30s` — see [docs/caching.md](docs/caching.md).

## Exit codes

| Code | Meaning |
|------|---------|
| 0 | Success / no differences / all resources valid |
| 1 | Differences found (diff only) |
| 2 | Error |
| 3 | Validation failed (validate only) |

## Caching

Six independent on-disk caches — Helm charts, remote kustomize resources, kustomize build outputs, clones of external GitRepository sources, downloaded validation schemas, CRD schemas converted from YAML — all under `~/.cache/fluxview/` by default, so a single volume mount covers them. Both sides of a `diff` share one warm copy of everything. Defaults: indexes, floating remote refs and floating external source resolutions re-check every `10m`, build outputs valid for `24h`, validation schemas never expire (they are version-pinned); pinned external source clones (tag/commit) are immutable and never re-fetched.

Details, caveats, and per-cache flags/env vars: [docs/caching.md](docs/caching.md).

## Docker

```bash
docker run --rm -v $(pwd):/repo -w /repo ghcr.io/banschikovde/fluxview:latest \
  build ks --path clusters/prod/flux/
```

The image runs as a fixed non-root user; caches live inside the container unless mounted. For running as your host user, read-only repo mounts, and CRD schemas: [docs/docker.md](docs/docker.md).

## CRD schemas

Kubernetes resource schemas are fetched automatically (kubeconform default registry, cached under `~/.cache/fluxview/schemas` — see `--kubernetes-version`). CRD schemas come from `--schema-dir` (default `/schemas/` or `./schemas/`): for Flux CRDs, download the schemas:

```bash
wget -qO- "https://github.com/fluxcd/flux2/releases/download/v2.9.1/crd-schemas.tar.gz" | tar xzf - -C ./schemas
```

For other CRDs (VictoriaMetrics, Kyverno, etc.), place their YAML manifests alongside.

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

fluxview:validate:
  image: ghcr.io/banschikovde/fluxview:latest
  before_script:
    - git config --global --add safe.directory "${CI_PROJECT_DIR}"
  script:
    - fluxview validate --path clusters/prod/flux/ --schema-dir ./schemas --output junit > fluxview-validate.xml
  artifacts:
    when: always
    reports:
      junit: fluxview-validate.xml
```

The validate job fails with exit 3 on invalid resources; `artifacts.when: always` still uploads the JUnit report, which GitLab renders in the merge-request test summary. Exit 2 means the run itself is broken (failed kustomize build, missing `spec.path`, unreadable schema dir) — no report is produced in that case.

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
