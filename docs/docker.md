# Docker

Pre-built image from GitHub Container Registry:

```bash
docker pull ghcr.io/banschikovde/fluxview:latest
```

Tags: `:latest`, plus a tag per release (version-pinned).

## Basic usage

Run against a local repo mounted as a volume:

```bash
docker run --rm -v $(pwd):/repo -w /repo ghcr.io/banschikovde/fluxview:latest \
  diff ks --path clusters/prod/flux/ --branch-orig master --strip-attrs helm.sh/chart,status --skip-crds
```

The repository mount can be read-only (`:ro`): git worktrees for `diff` are written to `/tmp`.

## Keeping caches between runs

The image runs as a fixed non-root user (65532:65532); the caches (Helm charts, kustomize remote resources, build outputs) default to `/home/fluxview/.cache/fluxview/` inside the container and disappear with it — mount or point them elsewhere to keep them between runs:

```bash
docker run --rm -v $(pwd):/repo -v fluxview-cache:/home/fluxview/.cache/fluxview \
  -w /repo ghcr.io/banschikovde/fluxview:latest build hr --path clusters/prod/flux/
```

See [caching.md](caching.md) for what each cache holds and its flags/env vars.

## Running as your host user

So a mounted cache is owned correctly, override `--user` and point the caches to a writable path — an arbitrary UID has no writable HOME in the image:

```bash
docker run --rm --user "$(id -u):$(id -g)" \
  -v $(pwd):/repo -e FLUXVIEW_HELM_CACHE_DIR=/tmp/helm-cache \
  -w /repo ghcr.io/banschikovde/fluxview:latest \
  build hr --path clusters/prod/flux/ --remote-cache-dir /tmp/ks-cache
```

## CRD schemas

CRD schemas for `validate` are not bundled — mount them via `-v /path/to/crds:/crds`:

```bash
docker run --rm -v $(pwd):/repo -v /path/to/crds:/crds \
  -w /repo ghcr.io/banschikovde/fluxview:latest \
  validate --path clusters/prod/flux/
```

## Building locally

```bash
docker build -t fluxview .
```
