package kustomize

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/banschikovde/fluxview/internal/git"
)

// Remote resource cache.
//
// Kustomize resolves http(s) entries in kustomization `resources:` by fetching
// them on every build (a plain HTTP GET for file URLs, a full git clone for
// repo-shaped URLs). The SDK offers no hook to intercept those fetches, so the
// cache works one level up: before a build, every kustomization under the
// repository root is scanned for remote references, each file-like URL is
// downloaded once into <cacheDir>/<sha256(url)>.yaml, and the filesystem layer
// served to kustomize (see rewritefs.go) reads those files back with URLs
// replaced by absolute cache paths. Kustomize then never sees a URL and makes
// no network requests of its own.
//
// The cache is fully independent of the Helm cache: its own directory
// (--remote-cache-dir) and its own TTL (--remote-cache-ttl), defaulting
// to a sibling directory under ~/.cache/fluxview/.
//
// Classification:
//   - pinned (release/download/vX.Y.Z, raw/…/<tag>/…, ?ref=<tag|sha>) —
//     immutable, no TTL check;
//   - floating (branch/HEAD-like refs, no recognizable version) — fresh within
//     the TTL, re-fetched after expiry, stale copy used with a warning when the
//     refresh fails;
//   - not a plain file (git/directory bases such as github.com/org/repo?ref=…,
//     `components:` URLs) — left untouched for kustomize itself, warned once.
//
// A URL that cannot be downloaded (and has no usable cached copy) is also left
// untouched, preserving the previous behavior exactly: kustomize attempts its
// own fetch and the build fails or warns as it did before the cache existed.

// maxRemoteResourceBytes caps a single remote resource download to keep a
// hostile or misbehaving URL from exhausting memory. Real CRD bundles are a few
// MB; 64 MiB is a generous ceiling.
const maxRemoteResourceBytes = 64 << 20

// DefaultRemoteCacheDir returns the remote resource cache directory:
// $XDG_CACHE_HOME/fluxview/kustomize-remote, else
// ~/.cache/fluxview/kustomize-remote. A sibling of the Helm cache under the
// same parent, so one volume mount covers both. The special values "off",
// "none" and "disabled" disable the cache (checked in WithRemoteCache).
func DefaultRemoteCacheDir() string {
	if base := os.Getenv("XDG_CACHE_HOME"); base != "" {
		return filepath.Join(base, "fluxview", "kustomize-remote")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "fluxview-remote-cache")
	}
	return filepath.Join(home, ".cache", "fluxview", "kustomize-remote")
}

// DefaultRemoteCacheTTL returns the freshness TTL for floating (non-pinned)
// remote resources: 10 minutes.
func DefaultRemoteCacheTTL() time.Duration {
	return 10 * time.Minute
}

// warnEnvTimeoutOnce keeps the invalid-env warning to a single line per
// process: DefaultRemoteCacheTimeout runs at flag registration and again
// wherever a remoteCache is constructed without an explicit timeout.
var warnEnvTimeoutOnce sync.Once

// DefaultRemoteCacheTimeout returns the per-request timeout for downloading
// remote resources: $FLUXVIEW_REMOTE_CACHE_TIMEOUT (Go duration, e.g. "30m"),
// else 30 seconds. An unparsable value warns once and falls back to the
// default; zero and negative values flow through and are normalized by
// newRemoteCache (zero = no limit).
func DefaultRemoteCacheTimeout() time.Duration {
	const def = 30 * time.Second
	v := os.Getenv("FLUXVIEW_REMOTE_CACHE_TIMEOUT")
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		warnEnvTimeoutOnce.Do(func() {
			fmt.Fprintf(os.Stderr, "Warning: invalid FLUXVIEW_REMOTE_CACHE_TIMEOUT %q, using %s\n", v, def)
		})
		return def
	}
	return d
}

// remoteCache downloads remote kustomize resources into an on-disk cache and
// resolves URLs to absolute cache paths for the rewriting filesystem.
// A Builder holds at most one; all entry points are safe for concurrent use
// (Build calls are typically sequential, but nothing relies on that).
type remoteCache struct {
	// dir is the absolute cache directory (the kustomize cache dir itself,
	// not a subdirectory of a shared root).
	dir string
	// ttl is how long a cached floating (non-pinned) resource stays fresh.
	// Zero means always re-fetch. Pinned URLs ignore it.
	ttl time.Duration
	// client fetches remote resources. A conservative timeout bounds a dead
	// remote; kustomize's own git operations default to 30s as well. Zero
	// disables the limit for slow links.
	client *http.Client

	mu sync.Mutex
	// resolved maps URL → absolute cache path, filled by prepare/ensure.
	resolved map[string]string
	// warned deduplicates per-URL warnings to one line per process.
	warned map[string]bool
	// prepared guards the one-shot scan of the repository root.
	prepared bool
}

// newRemoteCache creates a remote resource cache rooted at cacheDir (used
// as-is). An explicitly empty cacheDir means "default" (same convention as the
// Helm cache), not "relative paths off the CWD". A negative ttl behaves like 0
// (always re-fetch); a negative timeout like 0 (no limit). The directory
// itself is created lazily on the first successful download, so constructing
// the cache never fails or creates directories for repositories without
// remote refs.
func newRemoteCache(cacheDir string, ttl, timeout time.Duration) *remoteCache {
	if cacheDir == "" {
		cacheDir = DefaultRemoteCacheDir()
	}
	// Anchor the directory absolutely: a relative dir (e.g. CI's
	// --remote-cache-dir .cache/kustomize-remote) resolves against
	// the process working directory here, once. Without this the paths
	// rewritten into kustomization files stay relative and kustomize would
	// resolve them against each kustomization's own directory instead.
	if abs, err := filepath.Abs(cacheDir); err == nil {
		cacheDir = abs
	}
	if ttl < 0 {
		fmt.Fprintf(os.Stderr, "Warning: negative kustomize remote cache TTL %s, treating as 0 (always refresh)\n", ttl)
		ttl = 0
	}
	if timeout < 0 {
		fmt.Fprintf(os.Stderr, "Warning: negative kustomize remote download timeout %s, treating as 0 (no limit)\n", timeout)
		timeout = 0
	}
	return &remoteCache{
		dir:      cacheDir,
		ttl:      ttl,
		client:   &http.Client{Timeout: timeout},
		resolved: make(map[string]string),
		warned:   make(map[string]bool),
	}
}

// prepare scans rootDir once for remote references in kustomization files and
// downloads everything cacheable. Failures are per-URL warnings, never fatal:
// an undownloadable URL is simply left for kustomize to handle as it always
// did.
func (c *remoteCache) prepare(ctx context.Context, rootDir string) {
	c.mu.Lock()
	if c.prepared {
		c.mu.Unlock()
		return
	}
	c.prepared = true
	c.mu.Unlock()

	fileRefs, dirRefs := scanRemoteRefs(rootDir)
	for _, u := range sortedKeys(fileRefs) {
		if err := ctx.Err(); err != nil {
			c.warnf("", "remote cache scan interrupted, kustomize will fetch remaining resources itself: %v", err)
			return
		}
		c.ensure(ctx, u)
	}
	for _, u := range sortedKeys(dirRefs) {
		// `components:` must resolve to directories (kustomize git-clones
		// them); a single cached file can never stand in, so these stay on
		// kustomize's own fetch path.
		c.warnf(u, "remote component %s is not a cacheable file resource; kustomize will fetch it itself on every build", u)
	}
}

// ensure resolves rawURL to a usable cache path, downloading or refreshing as
// needed. It returns ("", false) when the URL should be left untouched.
func (c *remoteCache) ensure(ctx context.Context, rawURL string) (string, bool) {
	c.mu.Lock()
	if p, ok := c.resolved[rawURL]; ok {
		c.mu.Unlock()
		return p, true
	}
	c.mu.Unlock()

	path := c.cachePath(rawURL)
	pinned := isPinnedURL(rawURL)

	if c.usable(path, pinned) {
		c.remember(rawURL, path)
		return path, true
	}

	if err := c.download(ctx, rawURL, path); err != nil {
		// Stale fallback (floating refs only reach here once expired; a pinned
		// file is usable whenever it parses): keep working offline with a
		// warning rather than failing the build — same pattern as the Helm
		// index cache.
		if c.validYAMLFile(path) {
			c.warnf(rawURL, "could not refresh remote resource %s (%v), using cached copy", rawURL, err)
			c.remember(rawURL, path)
			return path, true
		}
		c.warnf(rawURL, "could not download remote resource %s (%v); kustomize will fetch it itself", rawURL, err)
		return "", false
	}

	c.remember(rawURL, path)
	return path, true
}

// remember records a successful resolution.
func (c *remoteCache) remember(rawURL, path string) {
	c.mu.Lock()
	c.resolved[rawURL] = path
	c.mu.Unlock()
}

// resolve returns the cached path for a URL previously resolved by prepare, or
// ("", false) when the URL is unknown to this cache instance. Read-only, used
// by the rewriting filesystem during builds.
func (c *remoteCache) resolve(rawURL string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.resolved[rawURL]
	return p, ok
}

// cachePath derives the cache file path for a URL: <dir>/<sha256(url)>.yaml.
func (c *remoteCache) cachePath(rawURL string) string {
	sum := sha256.Sum256([]byte(rawURL))
	return filepath.Join(c.dir, hex.EncodeToString(sum[:])+".yaml")
}

// usable reports whether the cached file for a URL can be served as-is: it
// must exist, parse as YAML (a corrupt file is re-downloaded, not served), and
// — for floating refs — be younger than the TTL.
func (c *remoteCache) usable(path string, pinned bool) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	if !pinned {
		if c.ttl <= 0 || time.Since(info.ModTime()) >= c.ttl {
			return false
		}
	}
	return c.validYAMLFile(path)
}

// validYAMLFile reports whether path exists and its content looks like a
// manifest file. Multi-document files (CRD bundles) are fine; empty,
// unparseable or non-manifest content is not — that is how a corrupted cache
// entry turns into a re-download instead of a cryptic kustomize error.
func (c *remoteCache) validYAMLFile(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return isManifestYAML(data)
}

// isManifestYAML reports whether data starts with a YAML document shaped like
// a kustomize resource: a mapping carrying both apiVersion and kind. Every k8s
// manifest and CRD bundle matches; bare scalars or sequences parse as YAML but
// are not manifests, and kustomize itself refuses kindless resources
// ("missing Resource metadata"), so admitting only this shape loses no cache
// coverage. This is also what keeps HTML pages that happen to parse as YAML
// (scalars, or mappings of tag garbage) out of the cache, preserving
// kustomize's git-clone fallback for repo/directory URLs.
func isManifestYAML(data []byte) bool {
	var node yaml.Node
	if err := yaml.NewDecoder(bytes.NewReader(data)).Decode(&node); err != nil {
		return false
	}
	if node.Kind != yaml.DocumentNode || len(node.Content) == 0 {
		return false
	}
	root := node.Content[0]
	if root.Kind != yaml.MappingNode {
		return false
	}
	var hasAPIVersion, hasKind bool
	for i := 0; i+1 < len(root.Content); i += 2 {
		switch root.Content[i].Value {
		case "apiVersion":
			hasAPIVersion = true
		case "kind":
			hasKind = true
		}
	}
	return hasAPIVersion && hasKind
}

// download fetches rawURL with a plain HTTP GET (GitHub release assets are
// served the same way — the client follows the redirect), validates the body
// as YAML, and writes it atomically alongside a .url sidecar for debuggability.
func (c *remoteCache) download(ctx context.Context, rawURL, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "fluxview")
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %s", resp.Status)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxRemoteResourceBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maxRemoteResourceBytes {
		return fmt.Errorf("larger than %d MiB", maxRemoteResourceBytes>>20)
	}
	if !isManifestYAML(data) {
		// Not a manifest file: most likely a repo/directory base that
		// kustomize resolves via git clone — nothing the file cache can do.
		return fmt.Errorf("not a YAML manifest")
	}

	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return err
	}
	if err := atomicWriteCachedFile(path, data); err != nil {
		return err
	}
	// Best-effort provenance sidecar; the cache works without it.
	_ = os.WriteFile(path+".url", []byte(rawURL+"\n"), 0o644)
	return nil
}

// atomicWriteCachedFile writes data to path via a temp file + rename so a
// concurrent fluxview process never observes a torn cache entry.
func atomicWriteCachedFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// warnf prints a warning to stderr once per key per cache instance.
func (c *remoteCache) warnf(key, format string, args ...any) {
	if key != "" {
		c.mu.Lock()
		if c.warned[key] {
			c.mu.Unlock()
			return
		}
		c.warned[key] = true
		c.mu.Unlock()
	}
	fmt.Fprintf(os.Stderr, "Warning: "+format+"\n", args...)
}

// scanRemoteRefs walks rootDir (skipping .git and nested git repository
// roots — external source clones cached inside the working tree are foreign
// content, their remote refs are not ours to prefetch) and collects remote
// references from every kustomization file: http(s) entries in `resources:`
// are file-fetchable, entries in `components:` are directory bases that only
// kustomize itself (via git) can resolve.
func scanRemoteRefs(rootDir string) (fileRefs, dirRefs map[string]bool) {
	fileRefs = make(map[string]bool)
	dirRefs = make(map[string]bool)
	walkRoot := filepath.Clean(rootDir)
	_ = filepath.WalkDir(rootDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entry: skip, keep walking
		}
		if d.IsDir() {
			if d.Name() == ".git" || (filepath.Clean(path) != walkRoot && git.IsRepoRoot(path)) {
				return filepath.SkipDir
			}
			return nil
		}
		if !isKustomizationFileName(d.Name()) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		var kust struct {
			Resources  []string `yaml:"resources"`
			Components []string `yaml:"components"`
		}
		if err := yaml.Unmarshal(data, &kust); err != nil {
			return nil // kustomize will report the parse error itself
		}
		for _, r := range kust.Resources {
			if isHTTPRef(r) {
				fileRefs[r] = true
			}
		}
		for _, comp := range kust.Components {
			if isHTTPRef(comp) {
				dirRefs[comp] = true
			}
		}
		return nil
	})
	return fileRefs, dirRefs
}

// isHTTPRef reports whether a kustomization reference is a remote http(s) URL.
func isHTTPRef(ref string) bool {
	return strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://")
}

// isKustomizationFileName matches the file names kustomize recognizes.
func isKustomizationFileName(name string) bool {
	switch name {
	case "kustomization.yaml", "kustomization.yml", "Kustomization":
		return true
	}
	return false
}

// isPinnedURL reports whether the URL carries an immutable-ish version marker.
// Pinned entries are served from the cache without any TTL check; everything
// else (branch/HEAD refs, unversioned paths) is treated as floating.
func isPinnedURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := u.Hostname()
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")

	switch {
	case host == "raw.githubusercontent.com" && len(segs) >= 4:
		// https://raw.githubusercontent.com/<owner>/<repo>/<ref>/<path…>
		return isImmutableRef(segs[2])
	case host == "github.com" && len(segs) >= 5 && segs[2] == "releases" && segs[3] == "download":
		// https://github.com/<owner>/<repo>/releases/download/<tag>/<file>
		return isImmutableRef(segs[4])
	}
	// ?ref=<tag|sha> on any other URL shape.
	if ref := u.Query().Get("ref"); ref != "" {
		return isImmutableRef(ref)
	}
	return false
}

// isImmutableRef matches a version tag or a git commit SHA — the two ref
// shapes that effectively never move. Branch names like main/master/HEAD do
// not match and stay floating.
//
// Version tags require at least two dotted numeric segments (x.y, x.y.z, with
// optional v-prefix and -rc suffix): single numbers ("20250101", "v2") are
// just as likely branch or date names and stay floating. SHAs must be 7–40 hex
// chars containing at least one a-f letter — an all-digit string is never
// treated as a SHA for the same reason.
func isImmutableRef(ref string) bool {
	if ref == "" {
		return false
	}
	// Commit SHA (7–40 hex chars with at least one letter).
	letters := strings.ContainsFunc(ref, func(r rune) bool { return r >= 'a' && r <= 'f' })
	if letters && len(ref) >= 7 && len(ref) <= 40 {
		allHex := true
		for _, r := range ref {
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
				allHex = false
				break
			}
		}
		if allHex {
			return true
		}
	}
	// Version tag with optional leading v: at least x.y.
	s := strings.TrimPrefix(ref, "v")
	digitsDots := s
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		digitsDots = s[:i]
	}
	segments := strings.Split(digitsDots, ".")
	if len(segments) < 2 {
		return false
	}
	for _, seg := range segments {
		if seg == "" {
			return false
		}
		for _, r := range seg {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

// sortedKeys returns map keys in stable order for deterministic warnings and
// download sequences.
func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
