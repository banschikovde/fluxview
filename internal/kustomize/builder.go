// Package kustomize provides kustomize build functionality via the Go SDK.
package kustomize

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
}

// BuilderOption configures a Builder created via NewBuilder.
type BuilderOption func(*Builder)

// WithRemoteCache enables the on-disk cache for remote resources referenced by
// kustomization files (http(s) entries in resources). The cache is independent
// of the Helm cache: its own directory (--kustomize-cache-dir, env
// FLUXVIEW_KUSTOMIZE_CACHE_DIR) and its own TTL (--kustomize-cache-ttl):
// pinned version URLs are cached without TTL, floating refs honor ttl
// (0 = always re-fetch). An empty cacheDir means DefaultCacheDir().
//
// With the cache enabled, builds serve rewritten kustomization content whose
// remote URLs point at cached files, so kustomize makes no network requests
// for them. URLs that cannot be cached (git/directory bases, failed downloads)
// keep their previous behavior — kustomize fetches them itself.
func WithRemoteCache(cacheDir string, ttl time.Duration) BuilderOption {
	return func(b *Builder) {
		b.remote = newRemoteCache(cacheDir, ttl)
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

// Build runs kustomize build in the given directory and returns YAML output.
// File access is restricted to the builder's rootDir to prevent path traversal
// attacks via malicious kustomization.yaml files.
//
// When a remote cache is configured, the repository is scanned once and remote
// resources are downloaded before the first build; kustomization reads then go
// through a rewriting filesystem layer that substitutes cached paths for URLs.
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
		// Download everything cacheable before kustomize touches any file, so
		// the rewriting layer only ever does map lookups during builds.
		b.remote.prepare(ctx, b.rootDir)
		// Cached remote resources are absolute paths outside rootDir; they
		// contain only public content this tool fetched on the repository's
		// behalf, so the restricted filesystem admits exactly that directory.
		extraRoots = append(extraRoots, b.remote.dir)
	}

	fsys := newRestrictedFs(b.rootDir, extraRoots...)
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
