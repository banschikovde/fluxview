package kustomize

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
// run. For periodic invocations on an unchanged tree (watchers, CI re-runs,
// repeated local validate) that work is pure waste.
//
// The build cache stores the final build output together with an input
// manifest recorded by the filesystem layer (see recordingfs.go): every file
// actually read during the build (path, size, mtime) plus every directory
// confirmed or listed (path, mtime). A cached entry is served only when a
// fresh stat of every recorded file and directory matches exactly. This is
// make/bazel-style mtime caching: any git checkout, edit or new file in a
// listed directory invalidates the affected entries, while unchanged trees
// are served without running kustomize at all.
//
// Cache key = tool/version salt + build directory + kustomization file name,
// size and mtime. The full manifest then verifies the rest of the tree. A
// TTL bounds staleness for scenarios mtimes cannot express, and the cache
// directory can be disabled entirely (--build-cache-dir=off).

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
// ~/.cache/fluxview. The special values "off", "none" and "disabled" disable
// the cache (checked in WithBuildCache).
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
// process (same convention as warnCacheTTLOnce).
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

// buildCache stores build outputs on disk keyed by their input state.
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

// buildFileInput is one file read during a build, with the stat signature the
// verification re-checks. Path is relative to the build's rootDir when the
// file lives inside it, absolute otherwise (remote-cache files).
type buildFileInput struct {
	Path        string `json:"path"`
	Size        int64  `json:"size"`
	ModTimeNano int64  `json:"mtime"`
}

// buildDirInput is one directory confirmed or listed during a build. Its
// mtime changes when direct entries are added, removed or renamed.
type buildDirInput struct {
	Path        string `json:"path"`
	ModTimeNano int64  `json:"mtime"`
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
// library version never hit. Computed once per process.
var buildCacheSalt = sync.OnceValue(calcBuildCacheSalt)

func calcBuildCacheSalt() string {
	h := sha256.New()
	fmt.Fprintf(h, "fluxview-build-cache-v1")
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

// entryPath derives the cache entry file path for a build directory and its
// kustomization file stat signature.
func (c *buildCache) entryPath(rootDir, dir, kustFile string, kustSize int64, kustMtimeNano int64) string {
	relDir := cachePathRelative(rootDir, normalizeFsPath(dir))
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%s|%d|%d", buildCacheSalt(), relDir, filepath.Base(kustFile), kustSize, kustMtimeNano)
	return filepath.Join(c.dir, hex.EncodeToString(h.Sum(nil))+".json")
}

// lookup returns the cached output for dir when one exists, has not expired
// and its full input manifest still matches the filesystem. Any problem is a
// miss; the caller then builds normally.
func (c *buildCache) lookup(rootDir, dir, kustFile string) ([]byte, bool) {
	if c.ttl <= 0 {
		return nil, false
	}
	st, err := os.Stat(kustFile)
	if err != nil {
		return nil, false
	}
	path := c.entryPath(rootDir, dir, kustFile, st.Size(), st.ModTime().UnixNano())

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

// verifyBuildInputs re-stats every recorded file and directory and reports
// whether all signatures still match exactly.
func verifyBuildInputs(rootDir string, entry buildCacheEntry) bool {
	for _, f := range entry.Files {
		path := f.Path
		if !filepath.IsAbs(path) {
			path = filepath.Join(rootDir, path)
		}
		st, err := os.Stat(path)
		if err != nil || st.Size() != f.Size || st.ModTime().UnixNano() != f.ModTimeNano {
			return false
		}
	}
	for _, d := range entry.Dirs {
		path := d.Path
		if !filepath.IsAbs(path) {
			path = filepath.Join(rootDir, path)
		}
		st, err := os.Stat(path)
		if err != nil || !st.IsDir() || st.ModTime().UnixNano() != d.ModTimeNano {
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

	// The kustomization file and the build directory are always inputs, even
	// if a future kustomize variant reads them through another path.
	st, err := os.Stat(kustFile)
	if err != nil {
		return
	}
	kust := buildFileInput{
		Path:        cachePathRelative(rootDir, normalizeFsPath(kustFile)),
		Size:        st.Size(),
		ModTimeNano: st.ModTime().UnixNano(),
	}
	if !slices.Contains(files, kust) {
		files = append(files, kust)
	}
	buildDir := buildDirInput{
		Path:        cachePathRelative(rootDir, normalizeFsPath(dir)),
		ModTimeNano: statDirMtimeNano(dir),
	}
	if buildDir.ModTimeNano != 0 && !slices.Contains(dirs, buildDir) {
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

	path := c.entryPath(rootDir, dir, kustFile, st.Size(), st.ModTime().UnixNano())
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

// statDirMtimeNano returns a directory's mtime, or 0 when it cannot be
// stat'ed (the caller then skips the record).
func statDirMtimeNano(dir string) int64 {
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return 0
	}
	return st.ModTime().UnixNano()
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
