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
func ParseHelmReleasesFromBytes(data []byte) ([]HelmRelease, error) {
	var results []HelmRelease

	docs := SplitYAMLDocuments(data)
	for _, doc := range docs {
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
		if meta.Kind != KindHelmRelease || !isHelmAPI(meta.APIVersion) {
			continue
		}

		var hr HelmRelease
		if err := yaml.Unmarshal([]byte(trimmed), &hr); err != nil {
			continue
		}
		results = append(results, hr)
	}

	return results, nil
}

// ParseHelmRepositoriesFromBytes parses HelmRepository resources from YAML
// output bytes. Used to extract HelmRepositories from kustomize build output
// so that namespace transformers are reflected (matching the HelmRelease's
// transformed namespace).
func ParseHelmRepositoriesFromBytes(data []byte) ([]HelmRepository, error) {
	var results []HelmRepository

	docs := SplitYAMLDocuments(data)
	for _, doc := range docs {
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
		if meta.Kind != KindHelmRepository || !isSourceAPI(meta.APIVersion) {
			continue
		}

		var repo HelmRepository
		if err := yaml.Unmarshal([]byte(trimmed), &repo); err != nil {
			continue
		}
		results = append(results, repo)
	}

	return results, nil
}

// ParseOCIRepositoriesFromBytes parses OCIRepository resources from YAML
// output bytes. Used to extract OCIRepositories from kustomize build output
// so that namespace transformers are reflected.
func ParseOCIRepositoriesFromBytes(data []byte) ([]OCIRepository, error) {
	var results []OCIRepository

	docs := SplitYAMLDocuments(data)
	for _, doc := range docs {
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
		if meta.Kind != KindOCIRepository || !isSourceAPI(meta.APIVersion) {
			continue
		}

		var repo OCIRepository
		if err := yaml.Unmarshal([]byte(trimmed), &repo); err != nil {
			continue
		}
		results = append(results, repo)
	}

	return results, nil
}

// ParseConfigMapsFromBytes parses ConfigMap resources from YAML output bytes.
// Used to extract ConfigMaps from kustomize build output.
func ParseConfigMapsFromBytes(data []byte) ([]ConfigMap, error) {
	var results []ConfigMap

	docs := SplitYAMLDocuments(data)
	for _, doc := range docs {
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
		if meta.APIVersion != "v1" || meta.Kind != "ConfigMap" {
			continue
		}

		var cm ConfigMap
		if err := yaml.Unmarshal([]byte(trimmed), &cm); err != nil {
			continue
		}
		results = append(results, cm)
	}

	return results, nil
}
