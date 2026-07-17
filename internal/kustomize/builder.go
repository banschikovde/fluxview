// Package kustomize provides kustomize build functionality via the Go SDK.
package kustomize

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

// Builder runs kustomize build via the Go SDK and returns YAML manifests.
type Builder struct {
	options    *krusty.Options
	kustomizer *krusty.Kustomizer
	rootDir    string
}

// NewBuilder creates a new kustomize Builder. LoadRestrictionsNone is used to
// support Flux's ../../base pattern, but file access is restricted to rootDir
// via a custom filesystem wrapper to prevent reading outside the repository.
func NewBuilder(rootDir string) *Builder {
	opts := krusty.MakeDefaultOptions()
	opts.LoadRestrictions = types.LoadRestrictionsNone
	return &Builder{
		options:    opts,
		kustomizer: krusty.MakeKustomizer(opts),
		rootDir:    rootDir,
	}
}

// Build runs kustomize build in the given directory and returns YAML output.
// File access is restricted to the builder's rootDir to prevent path traversal
// attacks via malicious kustomization.yaml files.
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

	fsys := newRestrictedFs(b.rootDir)

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
type restrictedFs struct {
	filesys.FileSystem
	rootDir string
}

func newRestrictedFs(rootDir string) filesys.FileSystem {
	abs, err := filepath.Abs(rootDir)
	if err != nil {
		abs = rootDir
	}
	// Resolve symlinks on rootDir to match EvalSymlinks results on file paths
	// (e.g. macOS /var → /private/var).
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return &restrictedFs{
		FileSystem: filesys.MakeFsOnDisk(),
		rootDir:    abs,
	}
}

// isWithinRoot checks if path stays within rootDir after resolution.
func (fs *restrictedFs) isWithinRoot(path string) bool {
	// fs.rootDir is already absolute + symlink-resolved at construction (see
	// newRestrictedFs), so don't re-resolve it on every file access — only
	// the path is resolved here (the actual security check).
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
	if !fs.isWithinRoot(path) {
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
	if !fs.isWithinRoot(path) {
		return fmt.Errorf("path %s is outside repository root", path)
	}
	return fs.FileSystem.WriteFile(path, data)
}

func (fs *restrictedFs) Mkdir(path string) error {
	if !fs.isWithinRoot(path) {
		return fmt.Errorf("path %s is outside repository root", path)
	}
	return fs.FileSystem.Mkdir(path)
}

func (fs *restrictedFs) MkdirAll(path string) error {
	if !fs.isWithinRoot(path) {
		return fmt.Errorf("path %s is outside repository root", path)
	}
	return fs.FileSystem.MkdirAll(path)
}

func (fs *restrictedFs) RemoveAll(path string) error {
	if !fs.isWithinRoot(path) {
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
