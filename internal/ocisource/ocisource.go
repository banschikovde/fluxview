// Package ocisource fetches Flux OCIRepository sources: it resolves the
// artifact reference (digest > semver > tag > the literal latest tag, the
// same priority as source-controller), downloads the selected layer and
// extracts it into a content-addressed on-disk cache.
//
// The cache layout mirrors internal/gitsource:
//
//	<cacheDir>/data/<sha256(url + "\0" + "digest:<manifest digest>")>/   extracted artifact + .fluxview-oci-source.json
//	<cacheDir>/refs/<sha256(url + "\0" + refSpec)>.json                  floating-ref → digest pointer
//	<cacheDir>/tmp/                                                       in-progress extractions
//
// A digest is the only immutable registry reference, so data directories
// are keyed by the manifest digest: a moved tag or a new semver winner
// resolves to a different digest and lands in a different directory;
// nothing is ever refreshed in place. The pointer files carry the
// tag/semver/latest → digest resolution and expire per the TTL. With a
// disabled cache directory every Ensure fetches into a private temp
// directory (removed by Close).
package ocisource

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/banschikovde/fluxview/internal/cachedir"
	"github.com/banschikovde/fluxview/internal/flux"
	"github.com/banschikovde/fluxview/internal/sourcecache"
)

// ociPrefix is the required URL scheme of an OCIRepository.
const ociPrefix = "oci://"

// seedFileName records the provenance of one artifact directory inside the
// directory itself; its presence marks the extraction as complete.
const seedFileName = ".fluxview-oci-source.json"

// DefaultCacheDir returns the OCI source cache directory: env
// FLUXVIEW_OCI_SOURCE_CACHE_DIR, else <cachedir.DefaultDir base>/
// oci-sources. The disable word "disabled" disables artifact reuse
// (checked in NewFetcher).
func DefaultCacheDir() string {
	return cachedir.DefaultDir("FLUXVIEW_OCI_SOURCE_CACHE_DIR", "oci-sources")
}

// DefaultTTL returns how long a floating-ref resolution (tag/semver/latest
// → digest pointer) stays fresh: env FLUXVIEW_OCI_SOURCE_CACHE_TTL (Go
// duration), else 10 minutes. Zero always re-resolves (fresh results are
// still written).
func DefaultTTL() time.Duration {
	return sourcecache.TTLFromEnv("FLUXVIEW_OCI_SOURCE_CACHE_TTL")
}

// DefaultTimeout returns the time limit for fetching one artifact
// (reference resolution + manifest + layer over the network): env
// FLUXVIEW_OCI_SOURCE_TIMEOUT (Go duration), else 5 minutes. Zero means
// no limit, for dead-slow links. Invalid values warn once and fall back
// to the default.
func DefaultTimeout() time.Duration {
	return sourcecache.DurationFromEnv(EnvTimeout, 5*time.Minute)
}

// EnvTimeout caps one artifact fetch (resolve + manifest + layer).
const EnvTimeout = "FLUXVIEW_OCI_SOURCE_TIMEOUT"

// EnvNoFetch turns OCI artifact fetching off for the run: no registry
// requests at all — Kustomizations pointing at an OCIRepository are
// resolved locally (the pre-1.9 behavior). The escape hatch for offline
// networks.
const EnvNoFetch = "FLUXVIEW_NO_OCI_SOURCE_FETCH"

// DefaultNoFetch reports whether OCI artifact fetching is off by default:
// env FLUXVIEW_NO_OCI_SOURCE_FETCH (1/true/yes/on).
func DefaultNoFetch() bool {
	return sourcecache.NoFetchFromEnv(EnvNoFetch)
}

// Fetcher downloads OCIRepository artifacts into the OCI source cache. A
// disabled cache directory still fetches — every artifact lands in a
// private temp directory with no reuse — because the Kustomization
// pipeline must not silently skip external content. Those temp
// directories accumulate in one per-Fetcher directory removed by Close.
// Always construct via NewFetcher; a nil *Fetcher is not a valid Fetcher
// (the shared registry client lives on it).
type Fetcher struct {
	// dir is the cache directory; empty means "no reuse" (temp dirs).
	dir string
	// ttl is how long a floating-ref → digest resolution stays fresh.
	// Zero always re-resolves. Digest-pinned refs ignore it.
	ttl time.Duration

	// timeout caps the network part of one artifact fetch; zero means no
	// limit. It never applies to waiting for another in-flight fetch of
	// the same digest.
	timeout time.Duration

	// temp is the base directory for cache-disabled fetches, created
	// lazily and removed by Close.
	temp sourcecache.TempBase

	// dedup serializes concurrent Ensure calls for the same artifact
	// directory within one process (shared plumbing, see sourcecache).
	dedup *sourcecache.Dedup

	// clientOnce guards the lazy construction of the shared registry
	// client (with its token cache and docker-config credentials).
	clientOnce sync.Once
	client     *authClientBundle
}

// NewFetcher creates a Fetcher over the given cache directory and TTL.
// A dir of "disabled" (or empty) disables artifact reuse.
// A negative ttl is normalized to zero (always re-resolve floating refs).
func NewFetcher(cacheDir string, ttl time.Duration, opts ...FetcherOption) *Fetcher {
	f := &Fetcher{
		dedup: sourcecache.NewDedup(),
	}
	if !cachedir.Disabled(cacheDir) {
		f.dir = cacheDir
	}
	if ttl < 0 {
		ttl = 0
	}
	f.ttl = ttl
	for _, opt := range opts {
		opt(f)
	}
	return f
}

// FetcherOption configures a Fetcher beyond its directory and TTL.
type FetcherOption func(*Fetcher)

// WithTimeout caps each network phase of an artifact fetch — the
// reference resolution and every fetch attempt (manifest + layer) each
// get the full budget, so waiting for a concurrent fetch of the same
// digest never eats into an attempt's own time. Zero or negative
// disables the limit.
func WithTimeout(d time.Duration) FetcherOption {
	return func(f *Fetcher) {
		if d < 0 {
			d = 0
		}
		f.timeout = d
	}
}

// Close removes the temporary artifact directories made while the cache
// was disabled. Cached artifacts (the data/ tree) are kept — they are the
// point of the cache. Safe on a nil Fetcher and safe to call twice.
func (f *Fetcher) Close() error {
	if f == nil {
		return nil
	}
	return f.temp.Remove()
}

// offTempDir returns the base directory for cache-disabled fetches,
// creating it on first use.
func (f *Fetcher) offTempDir() (string, error) {
	return f.temp.Dir("fluxview-ocisource-*")
}

// Ensure returns a local directory holding the extracted artifact content
// requested by repo (spec.ref resolved per Flux priority: digest > semver
// > tag > the literal latest tag), fetching it into the cache when needed.
// The directory is immutable for a given (url, digest, selected layer)
// triple; callers may read from it freely but must not modify it. Without
// a usable cache directory every fetch lands in a private temp directory
// (removed by Close).
func (f *Fetcher) Ensure(ctx context.Context, repo flux.OCIRepository) (string, error) {
	url := strings.TrimSpace(repo.Spec.URL)
	if url == "" {
		return "", fmt.Errorf("OCIRepository %s/%s has no spec.url",
			repo.Metadata.Namespace, repo.Metadata.Name)
	}
	if !strings.HasPrefix(url, ociPrefix) {
		return "", fmt.Errorf("OCIRepository %s/%s url %q must start with %s",
			repo.Metadata.Namespace, repo.Metadata.Name, url, ociPrefix)
	}
	if p := repo.Spec.Provider; p != "" && p != "generic" {
		return "", fmt.Errorf("OCIRepository %s/%s provider %q is not supported (only \"generic\"; %s needs in-cluster workload identity)",
			repo.Metadata.Namespace, repo.Metadata.Name, p, p)
	}
	base := strings.TrimPrefix(url, ociPrefix)
	if err := validateBase(base); err != nil {
		return "", fmt.Errorf("OCIRepository %s/%s url %q: %w",
			repo.Metadata.Namespace, repo.Metadata.Name, url, err)
	}

	refSpec := repo.Spec.Ref.RefString()

	// Each network phase gets its own timeout scope (see WithTimeout):
	// the reference resolution here, and every fetch attempt below — a
	// retry after a failed concurrent fetch starts with the full budget,
	// not the residue of the wait.
	resolveCtx, cancelResolve := f.timeoutCtx(ctx)
	defer cancelResolve()
	resolved, resolvedFresh, err := f.resolveRef(resolveCtx, url, base, repo)
	if err != nil {
		return "", f.explainTimeout(err)
	}

	if f == nil || f.dir == "" {
		dir, err := f.ensureUncached(ctx, url, base, resolved, repo)
		return dir, f.explainTimeout(err)
	}

	// The data key includes the layer selector's mediaType: two
	// OCIRepositories may share one artifact but extract different layers.
	// operation is excluded — extract and copy select identically (the
	// kustomize pipeline always untars).
	dir := filepath.Join(f.dir, "data", dataKey(url, resolved, repo.Spec.LayerSelector))
	if sourcecache.SeedValid(dir, seedFileName) {
		f.rememberPointer(url, refSpec, resolved, resolvedFresh)
		return dir, nil
	}

	if err := f.fetchSynced(ctx, url, base, resolved, repo, dir); err != nil {
		return "", f.explainTimeout(err)
	}
	f.rememberPointer(url, refSpec, resolved, resolvedFresh)
	return dir, nil
}

// timeoutCtx applies the fetch timeout to ctx; without a configured
// timeout (zero) it passes ctx through.
func (f *Fetcher) timeoutCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if f == nil || f.timeout <= 0 {
		return ctx, func() {
			// No timeout configured — there is nothing to cancel.
		}
	}
	return context.WithTimeout(ctx, f.timeout)
}

// explainTimeout names the timeout knobs when a fetch died on its
// deadline, so a hung registry reads as configuration, not a mystery.
func (f *Fetcher) explainTimeout(err error) error {
	if err == nil || f == nil || f.timeout <= 0 || !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%v (exceeded the artifact fetch timeout; raise it via --oci-source-timeout or %s, or set 0 to disable the limit)", err, EnvTimeout)
}

// validateBase rejects tag or digest inside the repository URL — Flux
// source-controller requires the version to come from spec.ref only.
func validateBase(base string) error {
	if strings.Contains(base, "@") {
		return fmt.Errorf("URL must not contain a digest; use spec.ref.digest")
	}
	if i := strings.LastIndex(base, "/"); i >= 0 && strings.Contains(base[i:], ":") {
		return fmt.Errorf("URL must not contain a tag; use spec.ref.tag or spec.ref.semver")
	}
	return nil
}

// resolveRef resolves the artifact digest: digest-pinned refs are their
// own resolved form; floating refs (tag/semver/latest) resolve through the
// cached pointer — the network is consulted only when it is missing or
// expired.
func (f *Fetcher) resolveRef(ctx context.Context, url, base string, repo flux.OCIRepository) (resolved string, fresh bool, err error) {
	if repo.Spec.Ref.IsPinned() {
		return repo.Spec.Ref.RefString(), false, nil
	}
	if f != nil && f.dir != "" {
		if p, ok := sourcecache.ReadPointer(f.dir, cacheKey(url, repo.Spec.Ref.RefString())); ok && sourcecache.Fresh(f.ttl, p) {
			return p.Resolved, true, nil
		}
	}
	digest, err := f.resolveFloating(ctx, base, repo)
	if err != nil {
		return "", false, err
	}
	return digestPrefix + digest, false, nil
}

// resolveFloating resolves a floating ref to the manifest digest over the
// network: semver lists the tags and picks the highest match, a tag (and
// the empty ref's literal "latest") resolves by name.
func (f *Fetcher) resolveFloating(ctx context.Context, base string, repo flux.OCIRepository) (string, error) {
	reg, err := f.repository(base, repo.Spec.Insecure)
	if err != nil {
		return "", err
	}
	ref := repo.Spec.Ref
	switch {
	case ref != nil && ref.Semver != "":
		tags, err := listTags(ctx, reg)
		if err != nil {
			return "", explainAuthStatus(fmt.Errorf("listing tags of %s: %w", ociPrefix+base, err), repo.Spec.SecretRef)
		}
		tag, err := pickSemverTag(tags, ref.Semver)
		if err != nil {
			return "", err
		}
		desc, err := reg.Resolve(ctx, tag)
		if err != nil {
			return "", explainAuthStatus(fmt.Errorf("resolving %s:%s: %w", ociPrefix+base, tag, err), repo.Spec.SecretRef)
		}
		return desc.Digest.String(), nil
	case ref != nil && ref.Tag != "":
		desc, err := reg.Resolve(ctx, ref.Tag)
		if err != nil {
			return "", explainAuthStatus(fmt.Errorf("resolving %s:%s: %w", ociPrefix+base, ref.Tag, err), repo.Spec.SecretRef)
		}
		return desc.Digest.String(), nil
	default:
		// No ref defaults to the literal latest tag — same as
		// source-controller (name.DefaultTag).
		desc, err := reg.Resolve(ctx, "latest")
		if err != nil {
			return "", explainAuthStatus(fmt.Errorf("resolving %s:latest: %w", ociPrefix+base, err), repo.Spec.SecretRef)
		}
		return desc.Digest.String(), nil
	}
}

// ensureUncached fetches the artifact once into a private temp directory —
// the no-cache path, where every Ensure gets its own copy (removed by
// Close). The fetch runs under its own timeout scope, symmetric to the
// cached path's per-attempt scope.
func (f *Fetcher) ensureUncached(ctx context.Context, url, base, resolved string, repo flux.OCIRepository) (string, error) {
	var parent string
	if f != nil {
		var err error
		if parent, err = f.offTempDir(); err != nil {
			return "", err
		}
	}
	dst, err := os.MkdirTemp(parent, "fetch-*")
	if err != nil {
		return "", fmt.Errorf("preparing OCI source fetch: %w", err)
	}
	netCtx, cancel := f.timeoutCtx(ctx)
	defer cancel()
	if err := f.fetchInto(netCtx, base, resolved, repo, dst); err != nil {
		os.RemoveAll(dst)
		return "", err
	}
	return dst, nil
}

// fetchSynced fetches the artifact into dir, deduplicating concurrent
// fetches of the same directory within this process (the
// content-addressed directory is the dedup key). ctx governs the wait for
// another in-flight fetch; each attempt — the first fetch and a retry
// after a failed winner alike — creates its own timeout scope, so the
// wait never eats into the attempt's budget.
func (f *Fetcher) fetchSynced(ctx context.Context, url, base, resolved string, repo flux.OCIRepository, dir string) error {
	return f.dedup.Do(ctx, dir,
		func() bool { return sourcecache.SeedValid(dir, seedFileName) },
		func() error {
			netCtx, cancel := f.timeoutCtx(ctx)
			defer cancel()
			return f.fetchAtomic(netCtx, base, resolved, repo, dir)
		})
}

// fetchAtomic extracts the artifact into a temp directory and publishes it
// with a rename, tolerating a concurrent winner of the same target.
func (f *Fetcher) fetchAtomic(ctx context.Context, base, resolved string, repo flux.OCIRepository, dir string) error {
	tmp, err := sourcecache.ScratchDir(f.dir, "fetch-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	if err := f.fetchInto(ctx, base, resolved, repo, tmp); err != nil {
		return err
	}

	return sourcecache.Publish(tmp, dir, func(d string) bool { return sourcecache.SeedValid(d, seedFileName) })
}

// fetchInto downloads the artifact at digest (resolved, "digest:<sha>")
// and extracts the layer selected by spec.layerSelector into dst, writing
// the seed file last — its presence marks the directory complete.
func (f *Fetcher) fetchInto(ctx context.Context, base, resolved string, repo flux.OCIRepository, dst string) error {
	reg, err := f.repository(base, repo.Spec.Insecure)
	if err != nil {
		return err
	}
	digest := strings.TrimPrefix(resolved, digestPrefix)

	manifest, err := fetchManifest(ctx, reg, digest)
	if err != nil {
		return explainAuthStatus(fmt.Errorf("fetching manifest of %s@%s: %w", ociPrefix+base, digest, err), repo.Spec.SecretRef)
	}
	layerDesc, err := selectLayer(manifest, repo.Spec.LayerSelector)
	if err != nil {
		return err
	}
	blob, err := reg.Fetch(ctx, layerDesc)
	if err != nil {
		return explainAuthStatus(fmt.Errorf("downloading layer of %s@%s: %w", ociPrefix+base, digest, err), repo.Spec.SecretRef)
	}
	defer blob.Close()

	if err := extractTarGz(blob, layerDesc, dst); err != nil {
		return fmt.Errorf("extracting layer of %s@%s: %w", ociPrefix+base, digest, err)
	}

	if err := sourcecache.WriteSeed(dst, seedFileName, sourcecache.Meta{
		URL:       ociPrefix + base,
		Ref:       repo.Spec.Ref.RefString(),
		Resolved:  resolved,
		FetchedAt: time.Now().Format(time.RFC3339),
	}); err != nil {
		return fmt.Errorf("recording OCI source seed: %w", err)
	}
	return nil
}

// cacheKey is the shared content key of a (url, ref spec) pair.
func cacheKey(url, refSpec string) string {
	return sourcecache.Key(url, refSpec)
}

// dataKey keys one extracted directory: the resolved manifest digest
// plus the selected layer media type.
func dataKey(url, resolved string, sel *flux.OCILayerSelector) string {
	mediaType := ""
	if sel != nil {
		mediaType = sel.MediaType
	}
	return sourcecache.Key(url, resolved, "layer:"+mediaType)
}

// rememberPointer records a floating-ref resolution (tag/semver/latest →
// manifest digest); nil-safe on a nil Fetcher (nothing to remember).
func (f *Fetcher) rememberPointer(url, refSpec, resolved string, fresh bool) {
	var dir string
	if f != nil {
		dir = f.dir
	}
	sourcecache.RememberPointer(dir, cacheKey(url, refSpec), url, refSpec, resolved, fresh)
}
