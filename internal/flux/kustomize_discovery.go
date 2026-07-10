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
// overlays (kustomization.yaml with apiVersion kustomize.config.k8s.io).
// It excludes the rootPath itself and directories that contain Flux Kustomization resources.
func DiscoverKustomizeDirs(rootPath string) ([]string, error) {
	var dirs []string
	absRoot, _ := filepath.Abs(rootPath)

	err := filepath.WalkDir(rootPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}

		// Skip the root directory itself.
		absPath, _ := filepath.Abs(path)
		if absPath == absRoot {
			return nil
		}

		// Check for kustomization.yaml in this directory.
		kustPath := filepath.Join(path, "kustomization.yaml")
		data, err := os.ReadFile(kustPath)
		if err != nil {
			return nil // No kustomization.yaml, skip.
		}

		// Parse to check if it's a native kustomize overlay (not Flux Kustomization).
		var kust nativeKustomization
		if err := yaml.Unmarshal(data, &kust); err != nil {
			return nil
		}

		// Native kustomize has apiVersion starting with "kustomize.config.k8s.io"
		// or no apiVersion at all (just `kind: Kustomization`).
		if isNativeKustomize(kust) {
			dirs = append(dirs, path)
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	return dirs, nil
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
