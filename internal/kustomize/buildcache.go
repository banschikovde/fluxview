package kustomize

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"
)

// Kustomize build output cache.
//
// A kustomize build is the single most expensive step of every fluxview
// command (~70% of CPU and allocations): the SDK re-parses every input file
// into YAML node trees, deep-copies them and re-serializes the result on each
// run. For repeated invocations that work is pure waste — CI re-runs, jobs
// that call fluxview several times, periodic validate on a mounted tree.
//
// The build cache stores the final build output together with an input
// manifest recorded by the filesystem layer (see recordingfs.go): every file
// read during the build (path, sha256 of content) plus a listing of every
// directory involved (sorted entry names). A cached entry is served only when
// re-hashing every recorded file and re-listing every recorded directory
// reproduces the manifest exactly. The cache is therefore CONTENT-addressed:
// it survives fresh git checkouts (mtimes change, content does not), which is
// what makes it useful across CI jobs — a job rebuilds only the subtrees its
// commit actually touched. Directory listings catch files appearing or
// disappearing (new resource pulled in by a glob or directory reference)
// without trusting directory mtimes.
//
// Cache key = tool/version salt + build directory + kustomization file name
// and content hash. The full manifest then verifies the rest of the tree. A
// TTL bounds staleness as a belt-and-braces safety net, and the cache can be
// disabled entirely (--build-cache-dir=off).

const (
	// maxBuildCacheEntryBytes skips caching absurdly large build outputs so a
	// hostile repository cannot fill the disk. Mirrors maxRemoteResourceBytes.
	maxBuildCacheEntryBytes = 64 << 20
	// maxBuildCacheEntries bounds the number of stored entries and
	// maxBuildCacheTotalBytes the disk footprint; opportunistic oldest-first
	// eviction starts beyond either bound.
	maxBuildCacheEntries    = 2048
	maxBuildCacheTotalBytes = 256 << 20
)

// DefaultBuildCacheDir returns the build cache directory: env
// FLUXVIEW_BUILD_CACHE_DIR, else a sibling of the other fluxview caches under
// ~/.cache/fluxview.
func DefaultBuildCacheDir() string {
	if dir := os.Getenv("FLUXVIEW_BUILD_CACHE_DIR"); dir != "" {
		return dir
	}
	if base := os.Getenv("XDG_CACHE_HOME"); base != "" {
		return filepath.Join(base, "fluxview", "kustomize-builds")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "fluxview-build-cache")
	}
	return filepath.Join(home, ".cache", "fluxview", "kustomize-builds")
}

// warnBuildCacheTTLOnce keeps the invalid-env warning to a single line per
// process (same convention as the Helm cache).
var warnBuildCacheTTLOnce sync.Once

// DefaultBuildCacheTTL returns how long a build cache entry stays usable:
// env FLUXVIEW_BUILD_CACHE_TTL (Go duration), else 24 hours. Zero or negative
// effectively disables serving from cache (always rebuild).
func DefaultBuildCacheTTL() time.Duration {
	const def = 24 * time.Hour
	v := os.Getenv("FLUXVIEW_BUILD_CACHE_TTL")
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		warnBuildCacheTTLOnce.Do(func() {
			fmt.Fprintf(os.Stderr, "Warning: invalid FLUXVIEW_BUILD_CACHE_TTL %q, using %s\n", v, def)
		})
		return def
	}
	return d
}

// cacheDirDisabled reports whether a cache directory flag value turns that
// cache off. Shared by the remote resource cache and the build cache so every
// cache has the same disable spell: "off", "none" or "disabled".
func cacheDirDisabled(dir string) bool {
	switch strings.ToLower(dir) {
	case "off", "none", "disabled":
		return true
	}
	return false
}

// buildCache stores build outputs on disk keyed by their input content.
// A Builder holds at most one; lookups never fail the build — any doubt
// (missing entry, mismatch, corruption, TTL) is a cache miss.
type buildCache struct {
	// dir is the absolute cache directory, created lazily on first store.
	dir string
	// ttl is how long an entry stays usable after it was written.
	// <= 0 never serves from cache.
	ttl time.Duration

	mu     sync.Mutex
	warned map[string]bool
	stored bool
	swept  bool
}

func newBuildCache(dir string, ttl time.Duration) *buildCache {
	if dir == "" {
		dir = DefaultBuildCacheDir()
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	return &buildCache{dir: abs, ttl: ttl, warned: make(map[string]bool)}
}

// warnf prints a warning to stderr once per key per cache instance.
func (c *buildCache) warnf(key, format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.warned[key] {
		return
	}
	c.warned[key] = true
	fmt.Fprintf(os.Stderr, "Warning: "+format+"\n", args...)
}

// buildFileInput is one file read during a build, identified by content.
// Path is relative to the build's rootDir when the file lives inside it,
// absolute otherwise (remote-cache files).
type buildFileInput struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// buildDirInput is one directory involved in a build, identified by its
// sorted direct entry names — stable across checkouts, unlike mtime, and it
// changes exactly when a file is added, removed or renamed inside.
type buildDirInput struct {
	Path    string   `json:"path"`
	Entries []string `json:"entries"`
}

// buildCacheEntry is the on-disk cache format: input manifest + output.
type buildCacheEntry struct {
	Salt     string           `json:"salt"`
	BuildDir string           `json:"buildDir"`
	KustFile string           `json:"kustFile"`
	Files    []buildFileInput `json:"files"`
	Dirs     []buildDirInput  `json:"dirs"`
	Output   []byte           `json:"output"`
}

// buildCacheSalt fingerprints the binary (module version, VCS revision and
// dirtiness, kustomize library version) so entries from a different tool or
// library version never hit. Computed once per process. The format version
// ("v2") invalidates entries from the earlier mtime-based manifest format.
var buildCacheSalt = sync.OnceValue(calcBuildCacheSalt)

func calcBuildCacheSalt() string {
	h := sha256.New()
	fmt.Fprintf(h, "fluxview-build-cache-v2")
	if info, ok := debug.ReadBuildInfo(); ok {
		fmt.Fprintf(h, "main=%s", info.Main.Version)
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" || s.Key == "vcs.modified" {
				fmt.Fprintf(h, "%s=%s", s.Key, s.Value)
			}
		}
		for _, dep := range info.Deps {
			if dep.Path == "sigs.k8s.io/kustomize/api" {
				fmt.Fprintf(h, "kustomize=%s", dep.Version)
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// cachePathRelative converts an absolute recorded path to rootDir-relative
// when it stays inside rootDir; paths outside (remote cache files) remain
// absolute. Shared by recordingFs.manifest and Builder.Build. rootDir is
// symlink-resolved first so an unresolved root (e.g. macOS /var vs
// /private/var) still matches the normalized recorded paths.
func cachePathRelative(rootDir, absPath string) string {
	if rootDir == "" {
		return absPath
	}
	if resolved, err := filepath.EvalSymlinks(rootDir); err == nil {
		rootDir = resolved
	}
	rel, err := filepath.Rel(rootDir, absPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return absPath
	}
	return rel
}

// sortInputs orders manifest slices deterministically (by path).
func sortInputs(files []buildFileInput, dirs []buildDirInput) {
	slices.SortFunc(files, func(a, b buildFileInput) int {
		return strings.Compare(a.Path, b.Path)
	})
	slices.SortFunc(dirs, func(a, b buildDirInput) int {
		return strings.Compare(a.Path, b.Path)
	})
}

// hashFileContent returns the hex sha256 of the file at path.
func hashFileContent(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), size, nil
}

// entryPath derives the cache entry file path for a build directory and the
// content hash of its kustomization file.
func (c *buildCache) entryPath(rootDir, dir, kustFile, kustHash string) string {
	relDir := cachePathRelative(rootDir, normalizeFsPath(dir))
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%s|%s", buildCacheSalt(), relDir, filepath.Base(kustFile), kustHash)
	return filepath.Join(c.dir, hex.EncodeToString(h.Sum(nil))+".json")
}

// lookup returns the cached output for dir when one exists, has not expired
// and its full input manifest still matches the filesystem. Any problem is a
// miss; the caller then builds normally.
func (c *buildCache) lookup(rootDir, dir, kustFile string) ([]byte, bool) {
	if c.ttl <= 0 {
		return nil, false
	}
	kustHash, _, err := hashFileContent(kustFile)
	if err != nil {
		return nil, false
	}
	path := c.entryPath(rootDir, dir, kustFile, kustHash)

	info, err := os.Stat(path)
	if err != nil {
		return nil, false
	}
	if time.Since(info.ModTime()) > c.ttl {
		_ = os.Remove(path)
		return nil, false
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var entry buildCacheEntry
	if json.Unmarshal(data, &entry) != nil {
		// Corrupt entry: remove so the next store rewrites it cleanly.
		_ = os.Remove(path)
		return nil, false
	}
	if entry.Salt != buildCacheSalt() || !verifyBuildInputs(rootDir, entry) {
		return nil, false
	}
	return entry.Output, true
}

// verifyBuildInputs re-hashes every recorded file and re-lists every recorded
// directory and reports whether all signatures still match exactly.
func verifyBuildInputs(rootDir string, entry buildCacheEntry) bool {
	for _, f := range entry.Files {
		path := f.Path
		if !filepath.IsAbs(path) {
			path = filepath.Join(rootDir, path)
		}
		hash, size, err := hashFileContent(path)
		if err != nil || size != f.Size || hash != f.SHA256 {
			return false
		}
	}
	for _, d := range entry.Dirs {
		path := d.Path
		if !filepath.IsAbs(path) {
			path = filepath.Join(rootDir, path)
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return false
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		slices.Sort(names)
		if !slices.Equal(names, d.Entries) {
			return false
		}
	}
	return true
}

// store writes the build output and the recorded input manifest atomically.
// A ttl of 0 still stores (the entry is simply never served by this process;
// a later run with a non-zero ttl can use it) — mirroring the remote cache,
// where "0" means "always bypass", not "don't write". Failures warn once and
// never fail the build; the result is simply uncached.
func (c *buildCache) store(rootDir, dir, kustFile string, rec *recordingFs, output []byte) {
	if rec == nil {
		return
	}
	if len(output) > maxBuildCacheEntryBytes {
		c.warnf("too-large", "build output %d bytes exceeds cache cap %d, not cached", len(output), maxBuildCacheEntryBytes)
		return
	}
	files, dirs, ok := rec.manifest(rootDir)
	if !ok {
		return
	}

	// The kustomization file is always an input, even if a future kustomize
	// variant reads it through another path.
	kustHash, kustSize, err := hashFileContent(kustFile)
	if err != nil {
		return
	}
	kust := buildFileInput{
		Path:   cachePathRelative(rootDir, normalizeFsPath(kustFile)),
		Size:   kustSize,
		SHA256: kustHash,
	}
	if !slices.Contains(files, kust) {
		files = append(files, kust)
	}
	buildDir := buildDirInput{
		Path:    cachePathRelative(rootDir, normalizeFsPath(dir)),
		Entries: listDirEntries(dir),
	}
	if buildDir.Entries != nil && !slices.ContainsFunc(dirs, func(d buildDirInput) bool { return d.Path == buildDir.Path }) {
		dirs = append(dirs, buildDir)
	}
	sortInputs(files, dirs)

	entry := buildCacheEntry{
		Salt:     buildCacheSalt(),
		BuildDir: cachePathRelative(rootDir, normalizeFsPath(dir)),
		KustFile: filepath.Base(kustFile),
		Files:    files,
		Dirs:     dirs,
		Output:   output,
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return
	}

	path := c.entryPath(rootDir, dir, kustFile, kustHash)
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		c.warnf("mkdir", "build cache directory %s unavailable: %v", c.dir, err)
		return
	}
	if err := atomicWriteCachedFile(path, data); err != nil {
		c.warnf("write", "could not write build cache entry %s: %v", path, err)
		return
	}
	c.mu.Lock()
	first := !c.stored
	c.stored = true
	c.mu.Unlock()
	if first {
		c.sweep()
	}
}

// listDirEntries returns the sorted direct entry names of dir, or nil when it
// cannot be listed (the caller then skips the record).
func listDirEntries(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	slices.Sort(names)
	return names
}

// sweep evicts oldest-first when the cache grew beyond maxBuildCacheEntries
// entries or maxBuildCacheTotalBytes on disk. Called opportunistically after
// the first store of a process.
func (c *buildCache) sweep() {
	c.mu.Lock()
	if c.swept {
		c.mu.Unlock()
		return
	}
	c.swept = true
	c.mu.Unlock()

	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return
	}
	type aged struct {
		path    string
		size    int64
		modTime time.Time
	}
	agedEntries := make([]aged, 0, len(entries))
	var total int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		agedEntries = append(agedEntries, aged{path: filepath.Join(c.dir, e.Name()), size: info.Size(), modTime: info.ModTime()})
		total += info.Size()
	}
	if int64(len(agedEntries)) <= maxBuildCacheEntries && total <= maxBuildCacheTotalBytes {
		return
	}
	slices.SortFunc(agedEntries, func(a, b aged) int {
		return a.modTime.Compare(b.modTime)
	})
	remaining := len(agedEntries)
	for _, e := range agedEntries {
		if remaining <= maxBuildCacheEntries && total <= maxBuildCacheTotalBytes {
			return
		}
		_ = os.Remove(e.path)
		total -= e.size
		remaining--
	}
}
