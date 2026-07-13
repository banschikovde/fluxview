package flux

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// nativeKustomization represents a native kustomize Kustomization resource
// (apiVersion: kustomize.config.k8s.io/v1beta1, not Flux).
type nativeKustomization struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
}

// DiscoverKustomizeDirs scans subdirectories under rootPath for native kustomize
// overlays (kustomization.yaml/yml/Kustomization with apiVersion kustomize.config.k8s.io).
// It excludes:
//   - the rootPath itself
//   - directories that contain Flux Kustomization resources
//   - subdirectories of already discovered kustomize dirs
//   - directories referenced as resources by another discovered kustomization
//     (e.g. sibling base/ referenced via resources: [../base])
func DiscoverKustomizeDirs(rootPath string) ([]string, error) {
	// First pass: discover ALL kustomize dirs without dedup.
	type kustEntry struct {
		path      string
		absPath   string
		resources []string // resolved resource paths
	}
	var entries []kustEntry

	// Resolve rootPath for consistent path comparison (macOS /var → /private/var).
	absRootResolved, _ := filepath.Abs(rootPath)
	if real, err := filepath.EvalSymlinks(absRootResolved); err == nil {
		absRootResolved = real
	}

	err := filepath.WalkDir(rootPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}

		absPath, _ := filepath.Abs(path)
		if real, err := filepath.EvalSymlinks(absPath); err == nil {
			absPath = real
		}
		if absPath == absRootResolved {
			return nil
		}

		kustData := readKustomizationFile(path)
		if kustData == nil {
			return nil
		}

		var kust nativeKustomization
		if err := yaml.Unmarshal(kustData, &kust); err != nil {
			return nil
		}
		if !isNativeKustomize(kust) {
			return nil
		}

		// Parse resources to track base/overlay references.
		resources := parseResourcePaths(kustData, path)

		entries = append(entries, kustEntry{
			path:      path,
			absPath:   absPath,
			resources: resources,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Build set of all referenced paths (bases referenced by overlays).
	referenced := make(map[string]bool)
	for _, e := range entries {
		for _, r := range e.resources {
			referenced[r] = true
		}
	}

	// Second pass: filter — keep only dirs that are NOT referenced by another
	// kustomization (they'll be built as part of that kustomization).
	var dirs []string
	discovered := make(map[string]bool)
	for _, e := range entries {
		// Skip if referenced by another kustomization via resources: field
		// (e.g. sibling base/ referenced as ../base). This is the primary
		// dedup mechanism and handles both nested and sibling patterns.
		if referenced[e.absPath] {
			continue
		}
		// Safety-net: also skip physical subdirectories of already discovered
		// kustomize dirs. The resources: check above is more precise, but this
		// catches edge cases where a kustomization.yaml exists in a subdirectory
		// without being referenced in resources: (unusual but possible).
		skip := false
		for parent := range discovered {
			if strings.HasPrefix(e.absPath, parent+string(filepath.Separator)) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}

		dirs = append(dirs, e.path)
		discovered[e.absPath] = true
	}

	return dirs, nil
}

// DiscoverKustomizationFileDirs returns all directories under rootPath that
// contain a kustomization file (kustomization.yaml/yml/Kustomization),
// regardless of the declared kind. The loose-file walker uses this to skip any
// such directory's files so they don't leak as raw, untransformed resources —
// this notably covers orphan kind: Component directories (not referenced by any
// Kustomization) whose kustomization.yaml and resource inputs would otherwise
// appear in the output. Only kind: Kustomization directories are actually built.
func DiscoverKustomizationFileDirs(rootPath string) ([]string, error) {
	var dirs []string
	err := filepath.WalkDir(rootPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if readKustomizationFile(path) != nil {
			dirs = append(dirs, path)
		}
		return nil
	})
	return dirs, err
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
		if real, err := filepath.EvalSymlinks(absRes); err == nil {
			absRes = real
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

// ParseHelmReleasesFromBytes parses HelmRelease resources from YAML output bytes.
// Used to extract HelmReleases from kustomize build output so that namespace/
// name transformers applied by kustomize (e.g. a top-level `namespace:` field
// in kustomization.yaml) are reflected, unlike parsing the raw HelmRelease
// file directly from disk.
// parseResourcesFromBytes is the generic implementation behind all
// ParseXxxFromBytes functions: split YAML → filter by kind/apiVersion → unmarshal.
func parseResourcesFromBytes[T any](data []byte, match func(kind, apiVersion string) bool) ([]T, error) {
	var results []T

	for _, doc := range SplitYAMLDocuments(data) {
		trimmed := strings.TrimSpace(doc)
		if trimmed == "" {
			continue
		}

		var meta struct {
			APIVersion string `yaml:"apiVersion"`
			Kind       string `yaml:"kind"`
		}
		if err := yaml.Unmarshal([]byte(trimmed), &meta); err != nil {
			continue
		}
		if !match(meta.Kind, meta.APIVersion) {
			continue
		}

		var item T
		if err := yaml.Unmarshal([]byte(trimmed), &item); err != nil {
			continue
		}
		results = append(results, item)
	}

	return results, nil
}

func ParseHelmReleasesFromBytes(data []byte) ([]HelmRelease, error) {
	return parseResourcesFromBytes[HelmRelease](data, func(kind, api string) bool {
		return kind == KindHelmRelease && isHelmAPI(api)
	})
}

func ParseHelmRepositoriesFromBytes(data []byte) ([]HelmRepository, error) {
	return parseResourcesFromBytes[HelmRepository](data, func(kind, api string) bool {
		return kind == KindHelmRepository && isSourceAPI(api)
	})
}

func ParseOCIRepositoriesFromBytes(data []byte) ([]OCIRepository, error) {
	return parseResourcesFromBytes[OCIRepository](data, func(kind, api string) bool {
		return kind == KindOCIRepository && isSourceAPI(api)
	})
}

func ParseConfigMapsFromBytes(data []byte) ([]ConfigMap, error) {
	return parseResourcesFromBytes[ConfigMap](data, func(kind, api string) bool {
		return api == "v1" && kind == "ConfigMap"
	})
}

func ParseSecretsFromBytes(data []byte) ([]Secret, error) {
	return parseResourcesFromBytes[Secret](data, func(kind, api string) bool {
		return api == "v1" && kind == "Secret"
	})
}
