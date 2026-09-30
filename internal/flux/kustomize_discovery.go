package flux

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/banschikovde/fluxview/internal/git"
)

// nativeKustomization represents a native kustomize Kustomization resource
// (apiVersion: kustomize.config.k8s.io/v1beta1, not Flux).
type nativeKustomization struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
}

// DiscoverKustomizeDirsAndFiles walks rootPath once and returns both:
//   - buildDirs: native kustomize overlays to build (selection and dedup
//     rules below).
//   - fileDirs: every directory containing a kustomization file of any kind
//     (native overlay, Flux Kustomization, or Component).
//
// Combining the two in a single walk avoids walking the tree and re-reading
// each kustomization.yaml twice at call sites that need both (the loose-file
// walkers in internal/cli).
//
// buildDirs excludes:
//   - the rootPath itself
//   - directories that contain Flux Kustomization resources
//   - subdirectories of already discovered kustomize dirs
//   - directories referenced as resources by another discovered kustomization
//     (e.g. sibling base/ referenced via resources: [../base])
func DiscoverKustomizeDirsAndFiles(ctx context.Context, rootPath string) (buildDirs, fileDirs []string, err error) {
	// Resolve rootPath for consistent path comparison (macOS /var → /private/var).
	absRootResolved, _ := filepath.Abs(rootPath)
	if resolved, err := filepath.EvalSymlinks(absRootResolved); err == nil {
		absRootResolved = resolved
	}
	walkRoot := filepath.Clean(rootPath)

	w := &kustDiscoveryWalk{
		ctx:             ctx,
		walkRoot:        walkRoot,
		absRootResolved: absRootResolved,
	}
	err = filepath.WalkDir(rootPath, w.visit)
	if err != nil {
		return nil, nil, err
	}

	return selectBuildDirs(w.entries), w.fileDirs, nil
}

// kustDiscoveryWalk accumulates what one discovery tree walk finds: every
// directory holding a kustomization file (fileDirs, keeping the loose-file
// walker out of kustomize inputs) and the parsed native-kustomize entries
// (resources resolved, for the later referenced-by dedup).
type kustDiscoveryWalk struct {
	ctx             context.Context
	walkRoot        string
	absRootResolved string
	fileDirs        []string
	entries         []kustEntry
}

// kustEntry is one native kustomize directory found by discovery.
type kustEntry struct {
	path      string
	absPath   string
	resources []string // resolved resource paths
}

// visit is the WalkDir callback: read errors on single entries are
// skipped best-effort (aborting would silently drop every overlay —
// strictly worse); context cancellation is honored during what can be a
// long tree walk and is the only intentional abort.
// Repository boundaries (.git, nested repo roots — external source clones
// cached inside the working tree) are never crossed. Chart roots are
// deliberately NOT skipped: unlike the raw resource parser, discovery
// must find kustomization files inside vendored charts (and register
// their directories, keeping the loose-file walker out of chart
// templates).
func (w *kustDiscoveryWalk) visit(path string, d fs.DirEntry, err error) error {
	if err != nil {
		return nil
	}
	if err := w.ctx.Err(); err != nil {
		return err
	}
	if !d.IsDir() {
		return nil
	}
	if d.Name() == ".git" {
		return filepath.SkipDir
	}
	if filepath.Clean(path) != w.walkRoot && git.IsRepoRoot(path) {
		return filepath.SkipDir
	}

	absPath, _ := filepath.Abs(path)
	if resolved, err := filepath.EvalSymlinks(absPath); err == nil {
		absPath = resolved
	}
	if absPath == w.absRootResolved {
		return nil
	}

	kustData := readKustomizationFile(path)
	if kustData == nil {
		return nil
	}
	// Any directory with a kustomization file is a "file dir" — used to
	// keep the loose-file walker out of kustomize inputs (Component,
	// Flux Kustomization, native overlays alike).
	w.fileDirs = append(w.fileDirs, path)

	var kust nativeKustomization
	if err := yaml.Unmarshal(kustData, &kust); err != nil {
		return nil
	}
	if !isNativeKustomize(kust) {
		return nil
	}

	w.entries = append(w.entries, kustEntry{
		path:      path,
		absPath:   absPath,
		resources: parseResourcePaths(kustData, path),
	})
	return nil
}

// selectBuildDirs keeps the directories that are NOT referenced by another
// kustomization (they will be built as part of that kustomization): the
// resources: set is the primary dedup (nested and sibling bases alike);
// physical subdirectories of already-selected dirs are a safety net for
// kustomization files unreferenced in resources: (unusual but possible).
func selectBuildDirs(entries []kustEntry) (buildDirs []string) {
	referenced := make(map[string]bool)
	for _, e := range entries {
		for _, r := range e.resources {
			referenced[r] = true
		}
	}

	discovered := make(map[string]bool)
	for _, e := range entries {
		if referenced[e.absPath] || underAnyDir(e.absPath, discovered) {
			continue
		}
		buildDirs = append(buildDirs, e.path)
		discovered[e.absPath] = true
	}
	return buildDirs
}

// underAnyDir reports whether path is a physical subdirectory of any of
// the already-discovered kustomize directories.
func underAnyDir(path string, dirs map[string]bool) bool {
	for parent := range dirs {
		if strings.HasPrefix(path, parent+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// readKustomizationFile reads the first found kustomization file in dir.
func readKustomizationFile(dir string) []byte {
	for _, name := range []string{"kustomization.yaml", "kustomization.yml", "Kustomization"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err == nil {
			return data
		}
	}
	return nil
}

// parseResourcePaths extracts resource paths from a kustomization.yaml and
// resolves them to absolute paths relative to the kustomization's directory.
func parseResourcePaths(data []byte, kustDir string) []string {
	var parsed struct {
		Resources []string `yaml:"resources"`
	}
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		return nil
	}

	var resolved []string
	for _, res := range parsed.Resources {
		absRes := filepath.Join(kustDir, res)
		absRes, err := filepath.Abs(absRes)
		if err != nil {
			continue
		}
		// Resolve symlinks for reliable path comparison.
		if resolvedPath, err := filepath.EvalSymlinks(absRes); err == nil {
			absRes = resolvedPath
		}
		resolved = append(resolved, absRes)
	}
	return resolved
}

// isNativeKustomize checks if the resource is a native kustomize Kustomization
// (not a Flux Kustomization).
func isNativeKustomize(kust nativeKustomization) bool {
	if kust.Kind != "Kustomization" {
		return false
	}
	// Native kustomize uses kustomize.config.k8s.io or has empty apiVersion.
	return kust.APIVersion == "" ||
		strings.HasPrefix(kust.APIVersion, "kustomize.config.k8s.io")
}

// parseResourcesFromBytes is the generic implementation behind the
// ParseXxxFromBytes functions: decode each document into a yaml.Node once,
// match by kind/apiVersion, then decode the same node into the target type.
// Documents that fail to decode are silently skipped (they may not be the
// target type); the function always succeeds.
func parseResourcesFromBytes[T any](data []byte, match func(kind, apiVersion string) bool) []T {
	var results []T
	eachMatchingResource(data, match, func(item T) bool {
		results = append(results, item)
		return false
	})
	return results
}

// eachMatchingResource is the shared core of the parseResourcesFromBytes
// family: it decodes each document into a yaml.Node once, matches by
// kind/apiVersion, and decodes the same node into the target type. fn runs
// for every successfully decoded resource, and the walk stops early once fn
// returns true, reporting whether that happened. Documents that fail to
// decode are silently skipped; the function always succeeds.
func eachMatchingResource[T any](data []byte, match func(kind, apiVersion string) bool, fn func(T) bool) bool {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var node yaml.Node
		if err := decoder.Decode(&node); err != nil {
			if err != io.EOF {
				// The message names the parseResourcesFromBytes family
				// this core implements; keep it stable — it is part of the
				// CLI's (undocumented but reviewed) stderr output.
				fmt.Fprintf(os.Stderr, "Warning: YAML parse error in parseResourcesFromBytes: %v\n", err)
			}
			return false
		}

		mapping := mappingFor(&node)
		if mapping == nil {
			continue
		}
		if !match(mapScalar(mapping, "kind"), mapScalar(mapping, "apiVersion")) {
			continue
		}

		var item T
		if err := node.Decode(&item); err != nil {
			continue
		}
		if fn(item) {
			return true
		}
	}
}

// ParsedResources holds the Flux source types extracted from kustomize build
// output bytes in a single pass. Unlike calling the per-type
// ParseXxxFromBytes functions one by one (one full pass over the output per
// type), ParseAllFromBytes parses each document once.
type ParsedResources struct {
	HelmReleases     []HelmRelease
	HelmRepositories []HelmRepository
	OCIRepositories  []OCIRepository
	GitRepositories  []GitRepository
	ConfigMaps       []ConfigMap
	Secrets          []Secret
}

// ParseAllFromBytes extracts every supported Flux source type from YAML
// output bytes in one pass: each document is decoded into a yaml.Node once
// and dispatched by (apiVersion, kind). Documents that fail to decode are
// silently skipped; the function always succeeds.
func ParseAllFromBytes(data []byte) *ParsedResources {
	res := &ParsedResources{}

	decoder := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var node yaml.Node
		if err := decoder.Decode(&node); err != nil {
			if err != io.EOF {
				fmt.Fprintf(os.Stderr, "Warning: YAML parse error in ParseAllFromBytes: %v\n", err)
			}
			return res
		}

		mapping := mappingFor(&node)
		if mapping == nil {
			continue
		}
		res.dispatch(mapScalar(mapping, "kind"), mapScalar(mapping, "apiVersion"), &node)
	}
}

// dispatch decodes one document node into the matching field. The node has
// been parsed once by the caller; the target-type decode reuses it.
func (r *ParsedResources) dispatch(kind, apiVersion string, node *yaml.Node) {
	switch {
	case kind == KindHelmRelease && isHelmAPI(apiVersion):
		var hr HelmRelease
		if err := node.Decode(&hr); err == nil {
			r.HelmReleases = append(r.HelmReleases, hr)
		}
	case kind == KindHelmRepository && isSourceAPI(apiVersion):
		var repo HelmRepository
		if err := node.Decode(&repo); err == nil {
			r.HelmRepositories = append(r.HelmRepositories, repo)
		}
	case kind == KindOCIRepository && isSourceAPI(apiVersion):
		var repo OCIRepository
		if err := node.Decode(&repo); err == nil {
			r.OCIRepositories = append(r.OCIRepositories, repo)
		}
	case kind == KindGitRepository && isSourceAPI(apiVersion):
		var repo GitRepository
		if err := node.Decode(&repo); err == nil {
			r.GitRepositories = append(r.GitRepositories, repo)
		}
	default:
		r.dispatchCoreKind(kind, apiVersion, node)
	}
}

// dispatchCoreKind decodes one core (non-source) kind document: ConfigMaps
// and Secrets at apiVersion v1.
func (r *ParsedResources) dispatchCoreKind(kind, apiVersion string, node *yaml.Node) {
	switch {
	case kind == "ConfigMap" && apiVersion == "v1":
		var cm ConfigMap
		if err := node.Decode(&cm); err == nil {
			r.ConfigMaps = append(r.ConfigMaps, cm)
		}
	case kind == "Secret" && apiVersion == "v1":
		var secret Secret
		if err := node.Decode(&secret); err == nil {
			r.Secrets = append(r.Secrets, secret)
		}
	}
}

func ParseConfigMapsFromBytes(data []byte) []ConfigMap {
	return parseResourcesFromBytes[ConfigMap](data, func(kind, api string) bool {
		return api == "v1" && kind == "ConfigMap"
	})
}

func ParseSecretsFromBytes(data []byte) []Secret {
	return parseResourcesFromBytes[Secret](data, func(kind, api string) bool {
		return api == "v1" && kind == "Secret"
	})
}

func ParseKustomizationsFromBytes(data []byte) []Kustomization {
	return parseResourcesFromBytes[Kustomization](data, func(kind, api string) bool {
		return kind == KindKustomization && isKustomizeAPI(api)
	})
}

func ParseGitRepositoriesFromBytes(data []byte) []GitRepository {
	return parseResourcesFromBytes[GitRepository](data, func(kind, api string) bool {
		return kind == KindGitRepository && isSourceAPI(api)
	})
}

// HasKustomizationsFromBytes reports whether data holds at least one Flux
// Kustomization document that fully decodes. It is the presence-check
// counterpart of ParseKustomizationsFromBytes: it stops at the first match
// and does not materialize the result slice.
func HasKustomizationsFromBytes(data []byte) bool {
	return eachMatchingResource(data, func(kind, api string) bool {
		return kind == KindKustomization && isKustomizeAPI(api)
	}, func(Kustomization) bool { return true })
}
