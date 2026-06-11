// Package kustomize provides kustomize build functionality via the Go SDK.
package kustomize

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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

// BuildMultiple runs kustomize build in multiple directories and concatenates the results.
func (b *Builder) BuildMultiple(dirs []string) ([]byte, error) {
	var results []string

	for _, dir := range dirs {
		output, err := b.Build(dir)
		if err != nil {
			return nil, fmt.Errorf("building %s: %w", dir, err)
		}

		trimmed := strings.TrimSpace(string(output))
		if trimmed != "" {
			results = append(results, trimmed)
		}
	}

	return []byte(strings.Join(results, "\n---\n")), nil
}

// BuildToString runs Build and returns the result as a string.
func (b *Builder) BuildToString(dir string) (string, error) {
	output, err := b.Build(dir)
	if err != nil {
		return "", err
	}
	return string(output), nil
}

// BuildFromBuffer runs kustomize build on in-memory content.
// This is useful for testing or when the kustomization is not on disk.
func BuildFromBuffer(kustomizationYAML []byte) ([]byte, error) {
	// Create a temporary directory.
	tmpDir, err := os.MkdirTemp("", "flux-diff-kustomize-*")
	if err != nil {
		return nil, fmt.Errorf("creating temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// Write the kustomization file.
	kustPath := filepath.Join(tmpDir, "kustomization.yaml")
	if err := os.WriteFile(kustPath, kustomizationYAML, 0644); err != nil {
		return nil, fmt.Errorf("writing kustomization file: %w", err)
	}

	builder := NewBuilder()
	return builder.Build(tmpDir)
}

// ValidateKustomization checks if a directory contains a valid kustomization file.
func ValidateKustomization(dir string) error {
	kustFile := findKustomizationFile(dir)
	if kustFile == "" {
		return fmt.Errorf("no kustomization file found in %s", dir)
	}

	// Try to read the file to verify it's accessible.
	content, err := os.ReadFile(kustFile)
	if err != nil {
		return fmt.Errorf("reading kustomization file: %w", err)
	}

	if len(content) == 0 {
		return fmt.Errorf("kustomization file is empty: %s", kustFile)
	}

	return nil
}

// RenderResources renders a kustomize ResMap to multi-document YAML.
func RenderResources(yamlOutput []byte) string {
	// Ensure the output has proper YAML document separators.
	output := strings.TrimSpace(string(yamlOutput))
	if output == "" {
		return ""
	}

	// Split into individual documents and reassemble with proper separators.
	docs := bytes.Split(yamlOutput, []byte("\n---\n"))
	var result []string
	for _, doc := range docs {
		trimmed := strings.TrimSpace(string(doc))
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}

	return strings.Join(result, "\n---\n")
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
