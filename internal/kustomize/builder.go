// Package kustomize provides kustomize build functionality via the Go SDK.
package kustomize

import (
	"fmt"
	"os"
	"path/filepath"

	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

// Builder runs kustomize build via the Go SDK and returns YAML manifests.
type Builder struct {
	// Options for kustomize build.
	options *krusty.Options
}

// NewBuilder creates a new kustomize Builder with default options.
func NewBuilder() *Builder {
	return &Builder{
		options: krusty.MakeDefaultOptions(),
	}
}

// NewBuilderWithOptions creates a new kustomize Builder with custom options.
func NewBuilderWithOptions(opts *krusty.Options) *Builder {
	return &Builder{
		options: opts,
	}
}

// Build runs kustomize build in the given directory and returns YAML output.
// The dir should be a directory containing a kustomization.yaml.
func (b *Builder) Build(dir string) ([]byte, error) {
	// Verify the kustomization file exists.
	kustFile := findKustomizationFile(dir)
	if kustFile == "" {
		return nil, fmt.Errorf("no kustomization file found in %s", dir)
	}

	// Use the on-disk filesystem.
	fsys := filesys.MakeFsOnDisk()

	// Create the kustomizer.
	k := krusty.MakeKustomizer(b.options)

	// Run kustomize build.
	resMap, err := k.Run(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("kustomize build in %s: %w", dir, err)
	}

	// Convert the result to YAML.
	yamlOutput, err := resMap.AsYaml()
	if err != nil {
		return nil, fmt.Errorf("serializing kustomize output: %w", err)
	}

	return yamlOutput, nil
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
