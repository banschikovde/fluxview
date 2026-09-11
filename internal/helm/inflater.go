// Package helm provides orchestration of Helm chart rendering via the Helm Go SDK.
package helm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
	"helm.sh/helm/v4/pkg/action"
	"helm.sh/helm/v4/pkg/chart"
	"helm.sh/helm/v4/pkg/chart/common"
	"helm.sh/helm/v4/pkg/chart/loader"
	"helm.sh/helm/v4/pkg/cli"
	"helm.sh/helm/v4/pkg/downloader"
	"helm.sh/helm/v4/pkg/getter"
	"helm.sh/helm/v4/pkg/helmpath"
	"helm.sh/helm/v4/pkg/registry"
	"helm.sh/helm/v4/pkg/release"
	"helm.sh/helm/v4/pkg/repo/v1"
	"helm.sh/helm/v4/pkg/storage"
	"helm.sh/helm/v4/pkg/storage/driver"
	k8syaml "sigs.k8s.io/yaml"

	"github.com/Masterminds/semver/v3"
	fluxtypes "github.com/banschikovde/fluxview/internal/flux"
	kustomizepkg "github.com/banschikovde/fluxview/internal/kustomize"
)

// Inflater renders Helm charts via the Helm Go SDK.
type Inflater struct {
	settings *cli.EnvSettings
	getters  getter.Providers
	// cacheDir is the root of the on-disk Helm cache (indexes, repositories.yaml,
	// downloaded chart tarballs).
	cacheDir string
	// indexTTL is how long a cached repository index.yaml stays fresh.
	// Zero means the index is re-downloaded on every use.
	indexTTL time.Duration
	// registered memoizes repoURL -> cache name so that several HelmReleases
	// sharing one repository validate the cached index only once per process.
	// InflateHelmRelease calls are sequential (inflateHelmReleasesShared loops
	// in order), so no locking is needed.
	registered map[string]string
	// ociDigestsPath is the on-disk tag→digest resolution cache for OCI refs.
	ociDigestsPath string
	// ociDigests is the in-memory copy of oci-digests.yaml, loaded once.
	ociDigests map[string]ociDigestEntry
	// ociTagsPath is the on-disk cache of constraint→selected-tag resolutions
	// for floating OCI versions (empty version or semver range).
	ociTagsPath string
	// ociTags is the in-memory copy of oci-tags.yaml, loaded once.
	ociTags map[string]ociTagEntry
	// ociClients memoizes registry clients per credentials+transport so that
	// several OCI HelmReleases reuse one client (and its auth cache) per
	// process instead of building a new one per chart.
	ociClients map[string]*registry.Client
}

// ociDigestEntry is one cached tag→digest resolution. Digests are immutable,
// but a tag may be moved by a push, so entries carry a timestamp and honor
// the index TTL.
type ociDigestEntry struct {
	Digest     string    `yaml:"digest"`
	ResolvedAt time.Time `yaml:"resolvedAt"`
}

// ociTagEntry is one cached constraint→tag selection (empty version or a
// semver range like ">=1.0.0"): new pushes may satisfy the constraint with a
// different tag, so entries honor the index TTL.
type ociTagEntry struct {
	Tag        string    `yaml:"tag"`
	ResolvedAt time.Time `yaml:"resolvedAt"`
}

// InflaterOption configures an Inflater created via NewInflater.
type InflaterOption func(*Inflater)

// WithCacheDir overrides the Helm cache directory (default: DefaultCacheDir()).
func WithCacheDir(dir string) InflaterOption {
	return func(in *Inflater) { in.cacheDir = dir }
}

// WithIndexTTL overrides the repository index cache TTL (default: DefaultIndexTTL()).
// Zero disables index caching (always fetch a fresh index).
func WithIndexTTL(ttl time.Duration) InflaterOption {
	return func(in *Inflater) { in.indexTTL = ttl }
}

// DefaultCacheDir returns the Helm cache directory: $FLUXVIEW_HELM_CACHE_DIR,
// else $XDG_CACHE_HOME/fluxview/helm, else ~/.cache/fluxview/helm.
func DefaultCacheDir() string {
	if dir := os.Getenv("FLUXVIEW_HELM_CACHE_DIR"); dir != "" {
		return dir
	}
	if base := os.Getenv("XDG_CACHE_HOME"); base != "" {
		return filepath.Join(base, "fluxview", "helm")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "fluxview-helm-cache")
	}
	return filepath.Join(home, ".cache", "fluxview", "helm")
}

// warnEnvTTLOnce keeps the invalid-env warning to a single line per process:
// DefaultIndexTTL runs both at flag registration (build and diff commands) and
// inside NewInflater. Tests reset it (warnEnvTTLOnce = sync.Once{}) to assert
// the warning order-independently — keep it a plain variable, not a func.
var warnEnvTTLOnce sync.Once

// DefaultIndexTTL returns the repository index cache TTL: $FLUXVIEW_HELM_INDEX_TTL
// (Go duration, e.g. "10m"), else 10 minutes. An unparsable value warns once
// and falls back to the default; a negative value flows through and is
// normalized by NewInflater.
func DefaultIndexTTL() time.Duration {
	const def = 10 * time.Minute
	v := os.Getenv("FLUXVIEW_HELM_INDEX_TTL")
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		warnEnvTTLOnce.Do(func() {
			fmt.Fprintf(os.Stderr, "Warning: invalid FLUXVIEW_HELM_INDEX_TTL %q, using %s\n", v, def)
		})
		return def
	}
	return d
}

// NewInflater creates a new Helm Inflater using the Go SDK.
// No external helm binary is required.
//
// The cache layout under the cache directory mirrors helm's own:
//   - repository/<name>-index.yaml — cached repo indexes
//   - repositories.yaml             — repo registry (URLs only, no credentials)
//   - content/<xx>/<sha256>.chart   — downloaded chart tarballs, HTTP and OCI
//     alike, keyed by the digest from the repo index or OCI manifest
//   - oci-digests.yaml              — OCI tag→digest resolutions (same TTL as
//     repo indexes)
//   - oci-tags.yaml                 — OCI constraint→tag selections for
//     floating versions (empty version or semver range), same TTL
//
// Note: the two digest-key families under content/ are distinct by design —
// HTTP downloads are keyed by the index digest (sha256 of the tarball), the
// OCI resolver by the manifest digest (sha256 of the OCI manifest). They never
// collide (different preimages) and coexist in one directory.
func NewInflater(opts ...InflaterOption) (*Inflater, error) {
	in := &Inflater{
		settings:   cli.New(),
		cacheDir:   DefaultCacheDir(),
		indexTTL:   DefaultIndexTTL(),
		registered: make(map[string]string),
		ociDigests: make(map[string]ociDigestEntry),
		ociTags:    make(map[string]ociTagEntry),
		ociClients: make(map[string]*registry.Client),
	}
	for _, opt := range opts {
		opt(in)
	}
	// An explicitly empty cache dir (e.g. `--helm-cache-dir=`) means "default",
	// not "relative paths off the CWD".
	if in.cacheDir == "" {
		in.cacheDir = DefaultCacheDir()
	}
	// A negative TTL (e.g. --helm-index-ttl=-5m) behaves like 0 — always
	// refresh — but normalized here so the "> 0" freshness check stays honest.
	if in.indexTTL < 0 {
		fmt.Fprintf(os.Stderr, "Warning: negative Helm index TTL %s, treating as 0 (always refresh)\n", in.indexTTL)
		in.indexTTL = 0
	}

	in.settings.RepositoryCache = filepath.Join(in.cacheDir, "repository")
	in.settings.RepositoryConfig = filepath.Join(in.cacheDir, "repositories.yaml")
	in.settings.ContentCache = filepath.Join(in.cacheDir, "content")
	in.ociDigestsPath = filepath.Join(in.cacheDir, "oci-digests.yaml")
	in.ociTagsPath = filepath.Join(in.cacheDir, "oci-tags.yaml")
	for _, dir := range []string{in.settings.RepositoryCache, in.settings.ContentCache} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("creating helm cache dir %s: %w", dir, err)
		}
	}
	in.getters = getter.All(in.settings)
	in.loadOCIDigests()
	in.loadOCITags()

	return in, nil
}

// repoCacheName derives a stable, filesystem-safe repository name from the URL.
// Two HelmRepository resources pointing at the same URL share one cache entry.
func repoCacheName(repoURL string) string {
	sum := sha256.Sum256([]byte(repoURL))
	return "fluxview-" + hex.EncodeToString(sum[:])[:16]
}

// registerRepo makes an HTTP HelmRepository resolvable through the SDK's
// repo/chart lookup: it records the repo in repositories.yaml (URL only —
// credentials stay in memory and are never written to the cache) and ensures a
// fresh-enough index in the repository cache.
//
// Resolving via <name>/<chart> instead of ChartPathOptions.RepoURL is what
// enables chart caching: the repo index carries the chart digest, so the SDK
// serves an already-downloaded tarball from its content cache instead of
// re-downloading it.
//
// If the index cannot be downloaded but a stale copy exists, the stale copy is
// used with a warning — a local diff tool should keep working offline rather
// than fail on a cache it can fill from disk.
func (in *Inflater) registerRepo(repoURL, username, password string) (string, error) {
	if name, ok := in.registered[repoURL]; ok {
		return name, nil
	}

	name := repoCacheName(repoURL)
	indexPath := filepath.Join(in.settings.RepositoryCache, helmpath.CacheIndexFile(name))

	// Load (or start) repositories.yaml. A corrupt file is treated as empty:
	// the registry is derived data, rewritten below.
	repoFile, err := repo.LoadFile(in.settings.RepositoryConfig)
	if err != nil || repoFile == nil {
		repoFile = repo.NewFile()
	}

	// A cached index counts as fresh only if it is within the TTL and still
	// parseable — LocateChart would parse it right after anyway, so validating
	// here costs nothing extra and turns corruption into a re-download instead
	// of a cryptic SDK error.
	fresh := false
	if info, statErr := os.Stat(indexPath); statErr == nil && in.indexTTL > 0 && time.Since(info.ModTime()) < in.indexTTL {
		if _, lErr := repo.LoadIndexFile(indexPath); lErr == nil {
			fresh = true
		}
	}

	if !fresh {
		// Credentials are passed to the index download via the in-memory
		// entry only; repositories.yaml persists the URL without them.
		dlEntry := &repo.Entry{Name: name, URL: repoURL, Username: username, Password: password}
		chartRepo, cErr := repo.NewChartRepository(dlEntry, in.getters)
		if cErr != nil {
			return "", fmt.Errorf("creating chart repository for %s: %w", repoURL, cErr)
		}
		chartRepo.CachePath = in.settings.RepositoryCache
		if _, dErr := chartRepo.DownloadIndexFile(); dErr != nil {
			if _, staleErr := os.Stat(indexPath); staleErr == nil {
				fmt.Fprintf(os.Stderr, "Warning: could not refresh index for %s (%v), using cached copy\n", repoURL, dErr)
			} else {
				return "", fmt.Errorf("downloading index for %s: %w", repoURL, dErr)
			}
		}
	}

	// Persist the entry (URL only) on the success path only, so a failed
	// registration leaves no trace in the cache.
	entry := &repo.Entry{Name: name, URL: repoURL}
	if existing := repoFile.Get(name); existing == nil || existing.URL != entry.URL {
		repoFile.Update(entry)
		data, mErr := k8syaml.Marshal(repoFile)
		if mErr != nil {
			return "", fmt.Errorf("marshaling repositories.yaml: %w", mErr)
		}
		if wErr := atomicWriteFile(in.settings.RepositoryConfig, data, 0644); wErr != nil {
			return "", fmt.Errorf("writing repositories.yaml: %w", wErr)
		}
	}

	in.registered[repoURL] = name
	return name, nil
}

// atomicWriteFile writes data to path via a temp file + rename so concurrent
// fluxview processes never observe a torn cache file (repositories.yaml,
// oci-digests.yaml, oci-tags.yaml).
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// --- OCI chart resolution with digest caching ---

// ociRef is a parsed OCI chart reference.
type ociRef struct {
	base   string // host/repo, no scheme, no tag/digest
	tag    string
	digest string // "sha256:..."
}

// parseOCIRef splits an OCI reference of the form
// oci://host/repo[:tag][@sha256:...]. The parsing is intentionally as naive as
// the helm SDK's own newReference (same last-colon tag heuristic); the
// registry client validates the result anyway.
func parseOCIRef(ref string) ociRef {
	r := strings.TrimPrefix(ref, "oci://")
	var out ociRef
	if i := strings.LastIndex(r, "@"); i >= 0 {
		out.digest = r[i+1:]
		r = r[:i]
	}
	if i := strings.LastIndex(r, ":"); i >= 0 && !strings.Contains(r[i:], "/") {
		out.tag = r[i+1:]
		r = r[:i]
	}
	out.base = r
	return out
}

// ociPlainHTTP reports whether the registry host is loopback. Local registries
// (e.g. oci://localhost:5000/...) conventionally serve plain HTTP; public
// registries are unaffected.
func ociPlainHTTP(ref string) bool {
	host := strings.SplitN(parseOCIRef(ref).base, "/", 2)[0]
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	return host == "localhost" || host == "::1" || strings.HasPrefix(host, "127.")
}

// ociClientFor returns a shared registry client for the given credentials and
// transport, building it on first use. Clients are keyed by creds so that
// credentials for one registry are never sent to another.
func (in *Inflater) ociClientFor(username, password string, plainHTTP bool) (*registry.Client, error) {
	key := fmt.Sprintf("%s\x00%s\x00%t", username, password, plainHTTP)
	if c, ok := in.ociClients[key]; ok {
		return c, nil
	}
	opts := []registry.ClientOption{registry.ClientOptEnableCache(true)}
	if plainHTTP {
		opts = append(opts, registry.ClientOptPlainHTTP())
	}
	if username != "" && password != "" {
		opts = append(opts, registry.ClientOptBasicAuth(username, password))
	}
	client, err := registry.NewClient(opts...)
	if err != nil {
		return nil, fmt.Errorf("creating registry client: %w", err)
	}
	in.ociClients[key] = client
	return client, nil
}

// loadOCIDigests reads oci-digests.yaml; a missing file is an empty cache, a
// corrupt one is treated as empty (the cache is derived, re-populated on use).
func (in *Inflater) loadOCIDigests() {
	data, err := os.ReadFile(in.ociDigestsPath)
	if err != nil {
		return
	}
	if err := k8syaml.Unmarshal(data, &in.ociDigests); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not parse %s (%v), rebuilding OCI digest cache\n", in.ociDigestsPath, err)
		in.ociDigests = make(map[string]ociDigestEntry)
	}
}

func (in *Inflater) saveOCIDigests() error {
	data, err := k8syaml.Marshal(in.ociDigests)
	if err != nil {
		return err
	}
	return atomicWriteFile(in.ociDigestsPath, data, 0644)
}

// cachedOCIDigest returns a fresh digest for the tag, if any.
func (in *Inflater) cachedOCIDigest(key string) (string, bool) {
	e, ok := in.ociDigests[key]
	if !ok || e.Digest == "" {
		return "", false
	}
	if in.indexTTL > 0 && time.Since(e.ResolvedAt) < in.indexTTL {
		return e.Digest, true
	}
	return "", false
}

// staleOCIDigest returns the cached digest regardless of age, for offline
// fallback when the registry cannot be reached.
func (in *Inflater) staleOCIDigest(key string) string {
	return in.ociDigests[key].Digest
}

func (in *Inflater) storeOCIDigest(key, digest string) {
	in.ociDigests[key] = ociDigestEntry{Digest: digest, ResolvedAt: time.Now()}
	if err := in.saveOCIDigests(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not persist OCI digest cache %s: %v\n", in.ociDigestsPath, err)
	}
}

// loadOCITags reads oci-tags.yaml; a missing file is an empty cache, a
// corrupt one is treated as empty.
func (in *Inflater) loadOCITags() {
	data, err := os.ReadFile(in.ociTagsPath)
	if err != nil {
		return
	}
	if err := k8syaml.Unmarshal(data, &in.ociTags); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not parse %s (%v), rebuilding OCI tag cache\n", in.ociTagsPath, err)
		in.ociTags = make(map[string]ociTagEntry)
	}
}

func (in *Inflater) saveOCITags() error {
	data, err := k8syaml.Marshal(in.ociTags)
	if err != nil {
		return err
	}
	return atomicWriteFile(in.ociTagsPath, data, 0644)
}

func (in *Inflater) cachedOCITag(key string) (string, bool) {
	e, ok := in.ociTags[key]
	if !ok || e.Tag == "" {
		return "", false
	}
	if in.indexTTL > 0 && time.Since(e.ResolvedAt) < in.indexTTL {
		return e.Tag, true
	}
	return "", false
}

func (in *Inflater) staleOCITag(key string) string {
	return in.ociTags[key].Tag
}

func (in *Inflater) storeOCITag(key, tag string) {
	in.ociTags[key] = ociTagEntry{Tag: tag, ResolvedAt: time.Now()}
	if err := in.saveOCITags(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not persist OCI tag cache %s: %v\n", in.ociTagsPath, err)
	}
}

// selectOCITag resolves a floating version (empty string or a semver
// constraint such as ">=1.0.0", OCIRepository spec.ref.semver) to a concrete
// tag by listing repository tags — the same logic the SDK's ValidateReference
// applies before handing the ref to the downloader. Selections are cached
// under the constraint key with the usual TTL and stale fallback.
func (in *Inflater) selectOCITag(base, version, username, password string, plainHTTP bool) (string, error) {
	key := base + ":" + version
	if tag, ok := in.cachedOCITag(key); ok {
		return tag, nil
	}

	client, err := in.ociClientFor(username, password, plainHTTP)
	if err != nil {
		return "", err
	}
	tags, terr := client.Tags(base)
	if terr != nil {
		if tag := in.staleOCITag(key); tag != "" {
			fmt.Fprintf(os.Stderr, "Warning: could not refresh tags for %s (%v), using cached selection %s\n", base, terr, tag)
			return tag, nil
		}
		return "", fmt.Errorf("listing tags for %s: %w", base, terr)
	}
	// Tags returns the list sorted descending by semver, so an empty version
	// selects the highest tag, matching the SDK.
	selected, merr := registry.GetTagMatchingVersionOrConstraint(tags, version)
	if merr != nil {
		return "", fmt.Errorf("selecting tag for %s:%s: %w", base, version, merr)
	}
	in.storeOCITag(key, selected)
	return selected, nil
}

// digestToKey converts a "sha256:<hex>" digest into the DiskCache key.
func digestToKey(digest string) ([sha256.Size]byte, error) {
	var key [sha256.Size]byte
	b, err := hex.DecodeString(strings.TrimPrefix(digest, "sha256:"))
	if err != nil || len(b) != sha256.Size {
		return key, fmt.Errorf("invalid digest %q", digest)
	}
	copy(key[:], b)
	return key, nil
}

// resolveOCIChart returns a local path to the chart archive for an OCI ref,
// fully cached: tag→digest resolutions live in oci-digests.yaml (TTL-bound,
// same knob as repo indexes), chart tarballs in the shared content cache
// keyed by the manifest digest. A warm run performs zero network requests;
// a digest-pinned ref is immutable and never re-resolves at all.
//
// This replaces the SDK's LocateChart for OCI refs: in helm v4.2.2 that path
// returns an empty digest to the downloader (ValidateReference resolves tags
// into a URL, not a digest), so the SDK re-downloads manifest and blob on
// every run and its content cache never hits.
func (in *Inflater) resolveOCIChart(chartRef, version, username, password string) (string, error) {
	r := parseOCIRef(chartRef)
	if r.tag != "" && version != "" && r.tag != version {
		return "", fmt.Errorf("chart reference and version mismatch: %s is not %s", version, r.tag)
	}
	tag := r.tag
	if tag == "" {
		tag = version
	}
	plainHTTP := ociPlainHTTP(chartRef)

	digest := r.digest
	if digest == "" {
		// A floating version (empty or a semver constraint) is resolved to a
		// concrete tag first — mirroring the SDK's ValidateReference, which
		// this resolver replaced.
		if _, serr := semver.NewVersion(tag); serr != nil {
			selected, err := in.selectOCITag(r.base, tag, username, password, plainHTTP)
			if err != nil {
				return "", err
			}
			tag = selected
		}
		key := r.base + ":" + tag
		if d, ok := in.cachedOCIDigest(key); ok {
			digest = d
		} else {
			client, err := in.ociClientFor(username, password, plainHTTP)
			if err != nil {
				return "", err
			}
			desc, rerr := client.Resolve(key)
			if rerr != nil {
				if d := in.staleOCIDigest(key); d != "" {
					fmt.Fprintf(os.Stderr, "Warning: could not refresh digest for %s (%v), using cached\n", key, rerr)
					digest = d
				} else {
					return "", fmt.Errorf("resolving %s: %w", key, rerr)
				}
			} else {
				digest = desc.Digest.String()
				in.storeOCIDigest(key, digest)
			}
		}
	}

	key, err := digestToKey(digest)
	if err != nil {
		return "", err
	}
	cache := &downloader.DiskCache{Root: in.settings.ContentCache}
	if pth, gerr := cache.Get(key, downloader.CacheChart); gerr == nil {
		return pth, nil
	}

	client, err := in.ociClientFor(username, password, plainHTTP)
	if err != nil {
		return "", err
	}
	g, gerr := in.getters.ByScheme("oci")
	if gerr != nil {
		return "", fmt.Errorf("creating oci getter: %w", gerr)
	}
	digestRef := "oci://" + r.base + "@" + digest
	data, gerr := g.Get(digestRef, getter.WithRegistryClient(client), getter.WithPlainHTTP(plainHTTP))
	if gerr != nil {
		return "", fmt.Errorf("downloading chart %s: %w", digestRef, gerr)
	}
	pth, perr := cache.Put(key, data, downloader.CacheChart)
	if perr != nil {
		return "", fmt.Errorf("caching chart %s: %w", digestRef, perr)
	}
	return pth, nil
}

// resolveOCIChartCancelable runs the (non-context-aware) resolveOCIChart while
// honoring ctx, mirroring locateChartCancelable.
func resolveOCIChartCancelable(ctx context.Context, in *Inflater, chartRef, version, username, password string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	type result struct {
		chartPath string
		err       error
	}
	ch := make(chan result, 1) // buffered: the goroutine always sends, never blocks
	go func() {
		cp, err := in.resolveOCIChart(chartRef, version, username, password)
		ch <- result{cp, err}
	}()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case r := <-ch:
		return r.chartPath, r.err
	}
}

// InflateHelmRelease inflates a Flux HelmRelease resource using the Helm Go SDK.
// It locates/downloads the chart from the given repo URL and renders templates
// equivalent to: helm template <name> <chart> --repo <url> --version <ver> --namespace <ns> --include-crds
func (in *Inflater) InflateHelmRelease(ctx context.Context, hr fluxtypes.HelmRelease, repoURL string, username, password string, configMaps []fluxtypes.ConfigMap, secrets []fluxtypes.Secret, repoRoot string) ([]byte, error) {
	chartName := hr.Spec.Chart.Spec.Chart

	// Set up the action configuration for template rendering (no k8s cluster needed).
	actionConfig := &action.Configuration{
		Releases:     storage.Init(driver.NewMemory()),
		Capabilities: common.DefaultCapabilities,
	}

	install := action.NewInstall(actionConfig)
	install.DryRunStrategy = action.DryRunClient
	// Include the chart's CRDs unless spec.install.crds is explicitly "Skip".
	// Matches Flux behavior: default (no field) and Create/CreateReplace include CRDs.
	install.IncludeCRDs = hr.Spec.Install == nil || hr.Spec.Install.CRDs != "Skip"
	install.ReleaseName = hr.Spec.ReleaseName
	if install.ReleaseName == "" {
		install.ReleaseName = hr.Metadata.Name
	}

	// Determine target namespace.
	namespace := hr.Metadata.Namespace
	if hr.Spec.TargetNamespace != "" {
		namespace = hr.Spec.TargetNamespace
	}
	install.Namespace = namespace

	// Chart resolution: OCI repos need special handling.
	// See helm/helm#10191: setting RepoURL for OCI causes index.yaml fetch failure.
	var chartRef string
	ociResolution := false
	switch {
	case strings.HasPrefix(chartName, "oci://"):
		// OCIRepository pattern: chartName is the full OCI reference
		// (URL + optional @digest). Use directly, don't append anything.
		chartRef = chartName
		install.ChartPathOptions.Version = hr.Spec.Chart.Spec.Version
		ociResolution = true

		// Registry client is still set on the install action: chart
		// dependencies pulled during rendering may need it.
		registryClient, err := in.ociClientFor(username, password, ociPlainHTTP(chartRef))
		if err != nil {
			return nil, err
		}
		actionConfig.RegistryClient = registryClient
		install.SetRegistryClient(registryClient)
	case strings.HasPrefix(repoURL, "oci://"):
		// HelmRepository type=oci: append chart name to repo URL.
		chartRef = strings.TrimSuffix(repoURL, "/") + "/" + chartName
		install.ChartPathOptions.Version = hr.Spec.Chart.Spec.Version
		ociResolution = true

		registryClient, err := in.ociClientFor(username, password, ociPlainHTTP(chartRef))
		if err != nil {
			return nil, err
		}
		actionConfig.RegistryClient = registryClient
		install.SetRegistryClient(registryClient)
	default:
		if repoURL == "" {
			// No repository involved: chartName is a local path (GitRepository
			// source resolved by the caller) that LocateChart loads from disk.
			chartRef = chartName
			install.ChartPathOptions.Version = hr.Spec.Chart.Spec.Version
			break
		}
		// Traditional HTTP HelmRepository: register it in the local helm config
		// and resolve via <repo>/<chart>. Unlike ChartPathOptions.RepoURL (which
		// re-downloads index.yaml and the tarball on every run), the repo/chart
		// lookup reads the cached index, learns the chart digest up front, and
		// serves an already-downloaded tarball from the content cache.
		repoName, err := in.registerRepo(repoURL, username, password)
		if err != nil {
			return nil, fmt.Errorf("preparing helm repository %s: %w", repoURL, err)
		}
		chartRef = repoName + "/" + chartName
		install.ChartPathOptions.Version = hr.Spec.Chart.Spec.Version
		// Credentials for the tarball download are passed via ChartPathOptions
		// (registerRepo keeps them out of the on-disk repositories.yaml).
		if username != "" && password != "" {
			install.ChartPathOptions.Username = username
			install.ChartPathOptions.Password = password
		}
	}

	// Locate the chart (downloads from repo if necessary). The Helm SDK's
	// LocateChart and its downloaders do not accept a context, so a cancelled
	// context can't truly abort an in-flight download — the cancelable helpers
	// stop waiting and return ctx.Err(), letting the caller exit while the
	// download finishes in a goroutine (the process terminates on Ctrl-C, so
	// the brief lingering download is acceptable for this CLI).
	var chartPath string
	var err error
	if ociResolution {
		chartPath, err = resolveOCIChartCancelable(ctx, in, chartRef, hr.Spec.Chart.Spec.Version, username, password)
		if err != nil {
			return nil, fmt.Errorf("locating chart %s: %w", chartRef, err)
		}
	} else {
		chartPath, err = locateChartCancelable(ctx, &install.ChartPathOptions, chartRef, in.settings)
		if err != nil {
			// A chart or version missing from the index while index caching is on is
			// most often staleness (the chart was published after the cached index).
			// The SDK's "try 'helm repo update'" advice does not apply to fluxview.
			if (errors.Is(err, repo.ErrNoChartVersion) || errors.Is(err, repo.ErrNoChartName)) && in.indexTTL > 0 {
				return nil, fmt.Errorf("locating chart %s: %w (cached index may be stale — retry with --helm-index-ttl=0)", chartRef, err)
			}
			return nil, fmt.Errorf("locating chart %s: %w", chartRef, err)
		}
	}

	// Load the chart.
	chartObj, err := loader.Load(chartPath)
	if err != nil {
		return nil, fmt.Errorf("loading chart from %s: %w", chartPath, err)
	}

	// Accessor abstracts v2/v3 chart differences and works uniformly whether the
	// chart was loaded from a directory or a .tgz archive — needed for resolving
	// chart.spec.valuesFiles (the chart's extra values files).
	chartAcc, err := chart.NewDefaultAccessor(chartObj)
	if err != nil {
		return nil, fmt.Errorf("accessing chart %s: %w", chartRef, err)
	}

	// Build the values map in Flux precedence order:
	//   chart's values.yaml (applied by the SDK as base)
	//   < chart.spec.valuesFiles   (extra values files inside the chart)
	//   < spec.valuesFrom          (ConfigMaps/Secrets, external)
	//   < spec.values              (inline, highest priority)
	values := make(map[string]interface{})

	for _, vf := range hr.Spec.Chart.Spec.ValuesFiles {
		if err := mergeChartValuesFile(values, chartAcc, vf); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to read chart values file %q for %s/%s: %v\n",
				vf, hr.Metadata.Namespace, hr.Metadata.Name, err)
		}
	}

	// valuesFrom (ConfigMaps/Secrets) override chart valuesFiles.
	for k, v := range fluxtypes.ResolveValuesFrom(hr, configMaps, secrets) {
		values[k] = v
	}

	// Inline values have the highest priority.
	for k, v := range hr.Spec.Values {
		values[k] = v
	}

	// Run template rendering.
	rel, err := install.RunWithContext(ctx, chartObj, values)
	if err != nil {
		return nil, fmt.Errorf("rendering chart %s: %w", chartRef, err)
	}

	// Extract manifest via accessor (Helm v4 returns Releaser interface).
	accessor, err := release.NewAccessor(rel)
	if err != nil {
		return nil, fmt.Errorf("accessing release manifest: %w", err)
	}

	manifest := accessor.Manifest()

	// Convert JSON-in-YAML to proper YAML format
	// Helm v4 sometimes renders large objects (especially CRDs) as JSON in YAML
	converted, err := ConvertJSONInYAMLToYAML([]byte(manifest))
	if err != nil {
		// If conversion fails, return original manifest
		return []byte(manifest), nil
	}

	// Apply postRenderers (kustomize patches) if configured.
	for _, pr := range hr.Spec.PostRenderers {
		if pr.Kustomize == nil || len(pr.Kustomize.Patches) == 0 {
			continue
		}
		patched, err := kustomizepkg.ApplyPatches(converted, pr.Kustomize.Patches, repoRoot)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to apply postRenderer patches for %s/%s: %v\n",
				hr.Metadata.Namespace, hr.Metadata.Name, err)
			continue
		}
		converted = patched
	}

	return converted, nil
}

// mergeChartValuesFile looks up a values file among the chart's miscellaneous
// files (chartObj.Files, populated by the Helm SDK uniformly for directory and
// .tgz-loaded charts) and merges its top-level keys into dst with a shallow
// merge — the same strategy used for valuesFrom/inline values. Resolving from
// the in-memory chart object (rather than re-reading from disk via chartPath)
// is required because chartPath can be a .tgz archive, not a directory, for
// HelmRepository-sourced charts. No filesystem access means no path-traversal
// surface. Missing files return an error so the caller can warn (non-fatal).
func mergeChartValuesFile(dst map[string]interface{}, acc chart.Accessor, name string) error {
	want := strings.TrimPrefix(name, "./")
	for _, f := range acc.Files() {
		if strings.TrimPrefix(f.Name, "./") == want {
			var parsed map[string]interface{}
			if err := yaml.Unmarshal(f.Data, &parsed); err != nil {
				return fmt.Errorf("parsing %s: %w", name, err)
			}
			for k, v := range parsed {
				dst[k] = v
			}
			return nil
		}
	}
	return fmt.Errorf("values file %q not found in chart", name)
}

// FindHelmRepoURL finds the URL for a HelmRepository referenced by a HelmRelease.
// For OCI repositories (type: oci), the URL is prefixed with oci:// as required
// by the Helm SDK.
func FindHelmRepoURL(repos []fluxtypes.HelmRepository, name, namespace string, secrets []fluxtypes.Secret) (string, string, string, error) {
	for _, repo := range repos {
		if repo.Metadata.Name == name && repo.Metadata.Namespace == namespace {
			url := repo.Spec.URL
			if repo.Spec.Type == "oci" && !strings.HasPrefix(url, "oci://") {
				url = strings.TrimPrefix(url, "https://")
				url = strings.TrimPrefix(url, "http://")
				url = "oci://" + url
			}

			username := ""
			password := ""
			if repo.Spec.SecretRef != nil {
				secretNS := namespace
				for _, secret := range secrets {
					if secret.Metadata.Name == repo.Spec.SecretRef.Name && secret.Metadata.Namespace == secretNS {
						username = secret.GetSecretValue("username")
						password = secret.GetSecretValue("password")
						break
					}
				}
			}

			return url, username, password, nil
		}
	}
	return "", "", "", fmt.Errorf("HelmRepository %s/%s not found", namespace, name)
}

// ConvertJSONInYAMLToYAML converts JSON-in-YAML format (e.g., metadata: {...})
// to proper YAML format (e.g., metadata:\n  annotations: ...).
// Helm v4 sometimes renders large objects like CRDs as JSON in YAML.
// Removes nil map values (e.g. annotations: null) produced by Helm templates
// with empty optional fields.
//
// Documents are split by text first (not yaml.Decoder) so that a single
// malformed document skips gracefully without dropping subsequent documents.
func ConvertJSONInYAMLToYAML(manifest []byte) ([]byte, error) {
	if len(bytes.TrimSpace(manifest)) == 0 {
		return nil, nil
	}

	rawDocs := fluxtypes.SplitYAMLText(manifest)

	var docs []string
	for _, rawDoc := range rawDocs {
		var doc interface{}
		if err := yaml.Unmarshal([]byte(rawDoc), &doc); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: skipping unparseable YAML document: %v\n", err)
			continue
		}
		if doc == nil {
			continue
		}
		doc = RemoveNilValues(doc)
		marshaled, err := yaml.Marshal(doc)
		if err != nil {
			continue
		}
		docs = append(docs, strings.TrimRight(string(marshaled), "\n"))
	}

	if len(docs) == 0 {
		return nil, nil
	}
	return []byte(strings.Join(docs, "\n---\n")), nil
}

// RemoveNilValues recursively removes map entries with nil values.
func RemoveNilValues(in interface{}) interface{} {
	switch v := in.(type) {
	case map[string]interface{}:
		result := make(map[string]interface{})
		for k, val := range v {
			if val == nil {
				continue
			}
			result[k] = RemoveNilValues(val)
		}
		return result
	case []interface{}:
		for i, val := range v {
			v[i] = RemoveNilValues(val)
		}
		return v
	default:
		return in
	}
}

// locateChartCancelable runs the (non-context-aware) ChartPathOptions.LocateChart
// while honoring ctx.
//
// The Helm SDK's LocateChart and its underlying chart downloaders do not accept
// a context, so a cancellation cannot interrupt an in-flight network download.
// Instead this stops waiting on ctx.Done() and returns ctx.Err(); the download
// keeps running in a goroutine that sends to a buffered channel (so it never
// blocks and terminates once the download returns). For a CLI the process is
// about to exit on Ctrl-C, so the brief lingering download is acceptable.
func locateChartCancelable(ctx context.Context, cpo *action.ChartPathOptions, name string, settings *cli.EnvSettings) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	type result struct {
		chartPath string
		err       error
	}
	ch := make(chan result, 1) // buffered: the goroutine always sends, never blocks
	go func() {
		cp, err := cpo.LocateChart(name, settings)
		ch <- result{cp, err}
	}()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case r := <-ch:
		return r.chartPath, r.err
	}
}
