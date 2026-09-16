// Package kustomize provides kustomize build functionality via the Go SDK.
package kustomize

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

// Builder runs kustomize build via the Go SDK and returns YAML manifests.
type Builder struct {
	options    *krusty.Options
	kustomizer *krusty.Kustomizer
	rootDir    string
	// remote caches remote resources referenced by kustomizations and
	// rewrites their URLs to local cache paths; nil when disabled.
	remote *remoteCache
	// buildCache caches kustomize build outputs on disk keyed by the input
	// file state; nil when disabled.
	buildCache *buildCache
	// allowMu guards allowRoots (AllowRoot may be called between Builds).
	allowMu sync.Mutex
	// allowRoots lists extra read-only roots admitted into every Build's
	// restricted filesystem, added lazily by AllowRoot (external git source
	// clones). Same mechanism as the remote cache directory.
	allowRoots []string
}

// BuilderOption configures a Builder created via NewBuilder.
type BuilderOption func(*Builder)

// WithRemoteCache enables the on-disk cache for remote resources referenced by
// kustomization files (http(s) entries in resources): --remote-cache-dir for
// the directory, --remote-cache-ttl for the TTL, --remote-cache-timeout for
// the per-request download timeout (0 = no limit, for slow links). Pinned
// version URLs are cached without TTL, floating refs honor the ttl (0 = always
// re-fetch). An empty cacheDir means DefaultRemoteCacheDir(); the values
// "off"/"none"/"disabled" disable the cache entirely (same spell as the build
// cache).
//
// With the cache enabled, builds serve rewritten kustomization content whose
// remote URLs point at cached files, so kustomize makes no network requests
// for them. URLs that cannot be cached (git/directory bases, failed downloads)
// keep their previous behavior — kustomize fetches them itself.
func WithRemoteCache(cacheDir string, ttl, timeout time.Duration) BuilderOption {
	return func(b *Builder) {
		if cacheDirDisabled(cacheDir) {
			return
		}
		b.remote = newRemoteCache(cacheDir, ttl, timeout)
	}
}

// WithBuildCache enables the on-disk cache of kustomize build outputs:
// --build-cache-dir / env FLUXVIEW_BUILD_CACHE_DIR, TTL via
// --build-cache-ttl / FLUXVIEW_BUILD_CACHE_TTL. A cached output is served
// only when re-hashing every recorded input file and re-listing every
// recorded directory reproduces the recorded manifest exactly — identity is
// content, so entries survive fresh checkouts while any edit or
// added/removed file invalidates the affected entries. A ttl of 0 always
// rebuilds but still refreshes entries for later runs; the dir values
// "off"/"none"/"disabled" disable the cache entirely.
func WithBuildCache(cacheDir string, ttl time.Duration) BuilderOption {
	return func(b *Builder) {
		if cacheDirDisabled(cacheDir) {
			return
		}
		b.buildCache = newBuildCache(cacheDir, ttl)
	}
}

// NewBuilder creates a new kustomize Builder. LoadRestrictionsNone is used to
// support Flux's ../../base pattern, but file access is restricted to rootDir
// via a custom filesystem wrapper to prevent reading outside the repository.
func NewBuilder(rootDir string, opts ...BuilderOption) *Builder {
	krustyOpts := krusty.MakeDefaultOptions()
	krustyOpts.LoadRestrictions = types.LoadRestrictionsNone
	b := &Builder{
		options:    krustyOpts,
		kustomizer: krusty.MakeKustomizer(krustyOpts),
		rootDir:    rootDir,
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// RootDir returns the repository root this Builder is confined to.
func (b *Builder) RootDir() string {
	return b.rootDir
}

// AllowRoot admits one more read-only root into every subsequent Build's
// restricted filesystem — used for external git source clone directories,
// which live outside rootDir exactly like the remote resource cache.
// Reading anywhere outside rootDir ∪ {remote cache dir} ∪ allowed roots
// stays impossible; writes stay confined to rootDir.
func (b *Builder) AllowRoot(dir string) {
	if dir == "" {
		return
	}
	b.allowMu.Lock()
	defer b.allowMu.Unlock()
	for _, existing := range b.allowRoots {
		if existing == dir {
			return // already admitted
		}
	}
	b.allowRoots = append(b.allowRoots, dir)
}

// extraRootSnapshot returns every extra read-only root for a Build.
func (b *Builder) extraRootSnapshot() []string {
	extra := make([]string, 0, 1+len(b.allowRoots))
	if b.remote != nil {
		extra = append(extra, b.remote.dir)
	}
	b.allowMu.Lock()
	extra = append(extra, b.allowRoots...)
	b.allowMu.Unlock()
	return extra
}

// Build runs kustomize build in the given directory and returns YAML output.
// File access is restricted to the builder's rootDir to prevent path traversal
// attacks via malicious kustomization.yaml files.
//
// When a remote cache is configured, the repository is scanned once and remote
// resources are downloaded before the first build — and before any build-cache
// lookup, so a refreshed floating resource (new content hash) invalidates the
// stale entry instead of being masked by it. Kustomization reads then go
// through a rewriting filesystem layer that substitutes cached paths for URLs.
//
// When a build cache is configured, outputs are served from disk while the
// input manifest recorded for them still matches the filesystem; otherwise the
// build runs through a recording filesystem layer and refreshes the entry.
//
// ctx is checked before the build starts; the kustomize library itself does
// not expose mid-build cancellation points, so a build already in flight runs
// to completion even if ctx is cancelled.
func (b *Builder) Build(ctx context.Context, dir string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	kustFile := findKustomizationFile(dir)
	if kustFile == "" {
		return nil, fmt.Errorf("no kustomization file found in %s", dir)
	}

	var extraRoots []string
	if b.remote != nil {
		// Download everything cacheable before kustomize touches any file —
		// and before the build-cache lookup below: a re-fetched floating
		// resource changes content, and the changed hash is exactly what
		// invalidates a build entry that consumed the old copy. Doing this
		// after the lookup would freeze floating resources until the build
		// entry itself expired.
		b.remote.prepare(ctx, b.rootDir)
	}
	// External git source clones (AllowRoot) and the remote cache directory
	// are absolute paths outside rootDir; they contain only public content
	// this tool fetched on the repository's behalf, so the restricted
	// filesystem admits exactly those directories, read-only.
	extraRoots = b.extraRootSnapshot()

	if b.buildCache != nil {
		if output, ok := b.buildCache.lookup(b.rootDir, dir, kustFile); ok {
			return output, nil
		}
	}

	fsys := newRestrictedFs(b.rootDir, extraRoots...)
	// The recording layer sits below the URL rewriter so it sees the final
	// local paths (rewritten remote URLs included) that the build reads.
	var rec *recordingFs
	if b.buildCache != nil {
		rec = newRecordingFs(fsys)
		fsys = rec
	}
	if b.remote != nil {
		fsys = b.remote.wrapFs(fsys)
	}

	resMap, err := b.kustomizer.Run(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("kustomize build in %s: %w", dir, err)
	}

	yamlOutput, err := resMap.AsYaml()
	if err != nil {
		return nil, fmt.Errorf("serializing kustomize output: %w", err)
	}

	if b.buildCache != nil {
		b.buildCache.store(b.rootDir, dir, kustFile, rec, yamlOutput)
	}

	return yamlOutput, nil
}

// restrictedFs wraps filesys.FileSystem to restrict all file access to rootDir.
// This prevents malicious kustomization.yaml from reading files outside the
// repository (e.g. resources: ../../../../etc/passwd) even though
// LoadRestrictionsNone is used (needed for Flux's ../../base pattern).
//
// extraRoots lists additional read-only roots (the remote resource cache
// directory) that kustomizations may reference via absolute paths.
type restrictedFs struct {
	filesys.FileSystem
	rootDir    string
	extraRoots []string
}

func newRestrictedFs(rootDir string, extraRoots ...string) filesys.FileSystem {
	abs, err := filepath.Abs(rootDir)
	if err != nil {
		abs = rootDir
	}
	// Resolve symlinks on rootDir to match EvalSymlinks results on file paths
	// (e.g. macOS /var → /private/var).
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	resolvedExtras := make([]string, 0, len(extraRoots))
	for _, extra := range extraRoots {
		resolvedExtras = append(resolvedExtras, resolveRootPath(extra))
	}
	return &restrictedFs{
		FileSystem: filesys.MakeFsOnDisk(),
		rootDir:    abs,
		extraRoots: resolvedExtras,
	}
}

// resolveRootPath absolutizes and symlink-resolves a directory used as a
// filesystem boundary.
func resolveRootPath(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

// isWithinRoot checks if path stays within rootDir (or one of the extra
// read-only roots) after resolution.
func (fs *restrictedFs) isWithinRoot(path string) bool {
	// fs.rootDir is already absolute + symlink-resolved at construction (see
	// newRestrictedFs), so don't re-resolve it on every file access — only
	// the path is resolved here (the actual security check).
	if isWithinResolvedRoot(path, fs.rootDir) {
		return true
	}
	for _, extra := range fs.extraRoots {
		if isWithinResolvedRoot(path, extra) {
			return true
		}
	}
	return false
}

// isWritableRoot checks if path stays within rootDir only: extra roots (the
// remote cache directory) are admitted read-only — a build must never mutate
// the cache, it is written exclusively by the downloader.
func (fs *restrictedFs) isWritableRoot(path string) bool {
	return isWithinResolvedRoot(path, fs.rootDir)
}

// isWithinResolvedRoot reports whether path resolves to within resolvedRoot,
// which must already be absolute and symlink-resolved. Only path is resolved.
func isWithinResolvedRoot(path, resolvedRoot string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		resolved = abs
	}
	return resolved == resolvedRoot ||
		strings.HasPrefix(resolved+string(filepath.Separator), resolvedRoot+string(filepath.Separator))
}

// IsPathWithinRoot checks if path resolves to within root (after symlink
// resolution on both path and root). Exported for reuse by ApplyPatches.
func IsPathWithinRoot(path, root string) bool {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	if resolved, err := filepath.EvalSymlinks(absRoot); err == nil {
		absRoot = resolved
	}
	return isWithinResolvedRoot(path, absRoot)
}

func (fs *restrictedFs) ReadFile(path string) ([]byte, error) {
	if !fs.isWithinRoot(path) {
		return nil, fmt.Errorf("path %s is outside repository root", path)
	}
	return fs.FileSystem.ReadFile(path)
}

func (fs *restrictedFs) Open(path string) (filesys.File, error) {
	if !fs.isWithinRoot(path) {
		return nil, fmt.Errorf("path %s is outside repository root", path)
	}
	return fs.FileSystem.Open(path)
}

func (fs *restrictedFs) Create(path string) (filesys.File, error) {
	if !fs.isWritableRoot(path) {
		return nil, fmt.Errorf("path %s is outside repository root", path)
	}
	return fs.FileSystem.Create(path)
}

func (fs *restrictedFs) IsDir(path string) bool {
	if !fs.isWithinRoot(path) {
		return false
	}
	return fs.FileSystem.IsDir(path)
}

func (fs *restrictedFs) Exists(path string) bool {
	if !fs.isWithinRoot(path) {
		return false
	}
	return fs.FileSystem.Exists(path)
}

func (fs *restrictedFs) ReadDir(path string) ([]string, error) {
	if !fs.isWithinRoot(path) {
		return nil, fmt.Errorf("path %s is outside repository root", path)
	}
	return fs.FileSystem.ReadDir(path)
}

func (fs *restrictedFs) WriteFile(path string, data []byte) error {
	if !fs.isWritableRoot(path) {
		return fmt.Errorf("path %s is outside repository root", path)
	}
	return fs.FileSystem.WriteFile(path, data)
}

func (fs *restrictedFs) Mkdir(path string) error {
	if !fs.isWritableRoot(path) {
		return fmt.Errorf("path %s is outside repository root", path)
	}
	return fs.FileSystem.Mkdir(path)
}

func (fs *restrictedFs) MkdirAll(path string) error {
	if !fs.isWritableRoot(path) {
		return fmt.Errorf("path %s is outside repository root", path)
	}
	return fs.FileSystem.MkdirAll(path)
}

func (fs *restrictedFs) RemoveAll(path string) error {
	if !fs.isWritableRoot(path) {
		return fmt.Errorf("path %s is outside repository root", path)
	}
	return fs.FileSystem.RemoveAll(path)
}

func (fs *restrictedFs) Glob(pattern string) ([]string, error) {
	// Glob patterns are hard to validate statically. Delegate and filter results.
	matches, err := fs.FileSystem.Glob(pattern)
	if err != nil {
		return nil, err
	}
	var filtered []string
	for _, m := range matches {
		if fs.isWithinRoot(m) {
			filtered = append(filtered, m)
		}
	}
	return filtered, nil
}

func (fs *restrictedFs) Walk(path string, walkFn filepath.WalkFunc) error {
	if !fs.isWithinRoot(path) {
		return fmt.Errorf("path %s is outside repository root", path)
	}
	return fs.FileSystem.Walk(path, walkFn)
}

func (fs *restrictedFs) CleanedAbs(path string) (filesys.ConfirmedDir, string, error) {
	dir, file, err := fs.FileSystem.CleanedAbs(path)
	if err != nil {
		return dir, file, err
	}
	if !fs.isWithinRoot(dir.String()) {
		return dir, "", fmt.Errorf("path %s is outside repository root", path)
	}
	return dir, file, nil
}

// findKustomizationFile returns the path to the kustomization file, or empty string if not found.
func findKustomizationFile(dir string) string {
	candidates := []string{
		"kustomization.yaml",
		"kustomization.yml",
		"Kustomization",
	}
	for _, name := range candidates {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}
