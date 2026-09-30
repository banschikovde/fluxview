package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/banschikovde/fluxview/internal/fsx"
	"github.com/banschikovde/fluxview/internal/git"
	"github.com/banschikovde/fluxview/internal/kustomize"
)

// overlayWalkOptions configures walkOverlaysAndLooseFiles for the two
// pipelines that share it: buildKustomizeOverlays (build) and
// buildSubdirectoriesAndLooseFiles (diff).
type overlayWalkOptions struct {
	// excludePaths prunes these directories and their subtrees from both
	// kustomize builds and loose-file reads (spec.path of Flux Kustomizations,
	// whose contents must not leak into the native-overlay output). Nil or
	// empty disables pruning.
	excludePaths map[string]bool
	// filterK8s keeps only documents that look like k8s resources (apiVersion
	// + kind) from loose files. The build pipeline filters; the diff pipeline
	// keeps every document.
	filterK8s bool
}

// walkOverlaysAndLooseFiles builds every native kustomize directory under root
// (see DiscoverKustomizeDirsAndFiles for the selection rules) and reads loose
// YAML files from directories not covered by any kustomization. The builder is
// passed in so all builds in one command share one configured Builder
// (including the remote resource cache).
//
// Discovery is the single memoized tree walk from scans: it returns both the
// native overlays to build and every kustomization-file directory (any kind)
// to keep the loose-file walker out of. If discovery failed, the walk silently
// proceeds with no known directories: builds are skipped and loose files are
// read from the whole tree. Callers needing a different fallback handle the
// discovery error beforehand via scans.kustDirsAndFiles.
//
// Loose-file reads are scoped to repoRoot via os.Root so a symlink resolving
// outside the repository is rejected (CWE-367). Skipped directories —
// kustomization directories and excluded paths — are pruned once on entry
// instead of being re-decided for every file (O(files×dirs) → O(dirs)).
//
// On error the outputs collected so far are still returned, so callers can
// choose between warn-and-continue (build) and failing (diff).
func walkOverlaysAndLooseFiles(ctx context.Context, scans *scanCache, builder *kustomize.Builder, root, repoRoot string, opts overlayWalkOptions, cache buildCache) ([][]byte, error) {
	kustomizeDirs, allKustFileDirs, err := scans.kustDirsAndFiles(ctx, root)

	var outputs [][]byte

	// Track ALL directories that have a kustomization file (any kind), not
	// just successfully built ones: the loose-file walker must skip them
	// entirely so their files never leak as raw resources — a failed build's
	// kustomization.yaml would appear as a garbage document, and an orphan
	// kind: Component dir would leak its inputs.
	kustDirs := make(map[string]bool)
	if err == nil {
		outputs = buildDiscoveredOverlays(ctx, builder, kustomizeDirs, opts.excludePaths, cache, outputs, kustDirs)
		// Also skip ANY directory containing a kustomization file (any kind),
		// including orphan kind: Component dirs not selected for building.
		for _, dir := range allKustFileDirs {
			kustDirs[dir] = true
		}
	}

	// Open repoRoot as a root-scoped FS so loose-file reads reject any symlink
	// resolving outside the repository. Scoped to repoRoot — not the walk
	// root — to keep legitimate intra-repo symlinks working, matching the
	// restrictedFs boundary used by kustomize builds.
	rootFS, rootErr := os.OpenRoot(repoRoot)
	if rootErr != nil {
		return outputs, fmt.Errorf("opening repo root %s: %w", repoRoot, rootErr)
	}
	defer rootFS.Close()

	loose := newLooseYAMLWalker(rootFS, repoRoot, root, kustDirs, opts)
	walkErr := filepath.Walk(root, loose.visit)

	return append(outputs, loose.outputs...), walkErr
}

// buildDiscoveredOverlays builds each discovered native kustomize
// directory (excluded paths pruned) and marks every one as a
// kustomization directory in kustDirs — regardless of build success — so
// the loose-file walker skips them entirely.
func buildDiscoveredOverlays(ctx context.Context, builder *kustomize.Builder, kustomizeDirs []string, excludePaths map[string]bool, cache buildCache, outputs [][]byte, kustDirs map[string]bool) [][]byte {
	for _, dir := range kustomizeDirs {
		kustDirs[dir] = true
		if isExcludedDir(dir, excludePaths) {
			continue
		}
		if output, ok := buildDirCached(ctx, builder, dir, cache); ok {
			outputs = append(outputs, output)
		}
	}
	return outputs
}

// looseYAMLWalker reads loose YAML files from directories not covered by
// any kustomization during one filepath.Walk. Pruning decisions are made
// once on directory entry instead of per file (O(files×dirs) → O(dirs)).
type looseYAMLWalker struct {
	rootFS       *os.Root
	repoRoot     string
	walkRoot     string
	kustDirs     map[string]bool
	filterK8s    bool
	excludePaths map[string]bool
	outputs      [][]byte
}

func newLooseYAMLWalker(rootFS *os.Root, repoRoot, root string, kustDirs map[string]bool, opts overlayWalkOptions) *looseYAMLWalker {
	return &looseYAMLWalker{
		rootFS:       rootFS,
		repoRoot:     repoRoot,
		walkRoot:     filepath.Clean(root),
		kustDirs:     kustDirs,
		filterK8s:    opts.filterK8s,
		excludePaths: opts.excludePaths,
	}
}

// visit is the filepath.Walk callback: prune kustomization/excluded/git
// directories, collect loose .yaml/.yml files (k8s-filtered when the
// caller asked). Read errors warn and skip — the walk continues.
func (w *looseYAMLWalker) visit(path string, info os.FileInfo, err error) error {
	if err != nil {
		return nil
	}
	if info.IsDir() {
		return w.visitDir(path)
	}
	w.visitFile(path)
	return nil
}

// visitDir decides whether a directory's subtree participates: directories
// with a kustomization file (regardless of build success/failure) and
// excluded paths are pruned, and the walk never crosses a repository
// boundary — the .git directory itself and nested git repository roots
// (external source clones cached inside the working tree) hold no loose
// fleet files.
func (w *looseYAMLWalker) visitDir(path string) error {
	if w.kustDirs[path] || isExcludedDir(path, w.excludePaths) {
		return filepath.SkipDir
	}
	if filepath.Base(path) == ".git" || (filepath.Clean(path) != w.walkRoot && git.IsRepoRoot(path)) {
		return filepath.SkipDir
	}
	return nil
}

// visitFile reads one loose YAML file (symlink-safe through the repo root
// FS — a symlink resolving outside the repository is rejected, CWE-367)
// and appends its documents to the outputs: k8s-filtered when filterK8s
// is set, raw otherwise.
func (w *looseYAMLWalker) visitFile(path string) {
	ext := strings.ToLower(filepath.Ext(path))
	if ext != ".yaml" && ext != ".yml" {
		return
	}
	data, err := fsx.ReadRootFile(w.rootFS, w.repoRoot, path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not read %s: %v\n", path, err)
		return
	}
	if w.filterK8s {
		// Only include documents that look like k8s resources.
		if filtered := filterK8sResources(data); filtered != nil {
			w.outputs = append(w.outputs, filtered)
		}
		return
	}
	w.outputs = append(w.outputs, data)
}

// buildKustomizeOverlays builds native kustomize overlays under clusterPath
// and collects loose k8s resources from directories without a kustomization
// file (see walkOverlaysAndLooseFiles). Failures are non-fatal: a warning
// goes to stderr and the outputs collected so far are returned.
func buildKustomizeOverlays(ctx context.Context, scans *scanCache, builder *kustomize.Builder, clusterPath string, excludePaths map[string]bool, cache buildCache) [][]byte {
	outputs, err := walkOverlaysAndLooseFiles(ctx, scans, builder, clusterPath, builder.RootDir(), overlayWalkOptions{excludePaths: excludePaths, filterK8s: true}, cache)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: walking %s: %v\n", clusterPath, err)
	}
	return outputs
}

// buildSubdirectoriesAndLooseFiles discovers native kustomize directories
// under sourcePath, builds each one via kustomize (applying
// namespace/transformers), then reads any loose YAML files not covered by a
// kustomization. When kustomize-directory discovery fails, it falls back to
// reading every YAML file under sourcePath recursively.
func buildSubdirectoriesAndLooseFiles(ctx context.Context, scans *scanCache, builder *kustomize.Builder, sourcePath, repoRoot string, cache buildCache) ([]byte, error) {
	// Discovery is memoized per sourcePath — several Flux Kustomizations
	// pointing at the same (or overlapping) spec.path used to re-walk each
	// time. Handle its failure here (recursive-read fallback); the walker
	// then reuses the cached result.
	if _, _, err := scans.kustDirsAndFiles(ctx, sourcePath); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: kustomize directory discovery failed for %s: %v\n", sourcePath, err)
		return readYAMLFilesRecursive(ctx, sourcePath, repoRoot)
	}

	outputs, err := walkOverlaysAndLooseFiles(ctx, scans, builder, sourcePath, repoRoot, overlayWalkOptions{}, cache)
	if err != nil {
		return nil, err
	}
	if len(outputs) == 0 {
		return nil, nil
	}
	docs := make([]string, len(outputs))
	for i, out := range outputs {
		docs[i] = string(out)
	}
	return []byte(strings.Join(docs, sectionSeparator)), nil
}
