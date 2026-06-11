package flux

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Parser discovers and parses Flux resources from the local filesystem.
type Parser struct {
	// RootPath is the root path of the git repository or cluster directory.
	RootPath string
}

// NewParser creates a new Parser rooted at the given path.
func NewParser(rootPath string) *Parser {
	return &Parser{RootPath: rootPath}
}

// ParseKustomizations discovers all Flux Kustomization resources under the root path.
func (p *Parser) ParseKustomizations(ctx context.Context) ([]Kustomization, error) {
	var result []Kustomization

	err := filepath.WalkDir(p.RootPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() || !isYAMLFile(path) {
			return nil
		}

		docs, err := p.parseFile(path)
		if err != nil {
			return fmt.Errorf("parsing file %s: %w", path, err)
		}

		for _, doc := range docs {
			ks, ok := doc.(Kustomization)
			if ok {
				result = append(result, ks)
			}
		}
		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("walking directory %s: %w", p.RootPath, err)
	}

	return result, nil
}

// ParseHelmReleases discovers all Flux HelmRelease resources under the root path.
func (p *Parser) ParseHelmReleases(ctx context.Context) ([]HelmRelease, error) {
	var result []HelmRelease

	err := filepath.WalkDir(p.RootPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() || !isYAMLFile(path) {
			return nil
		}

		docs, err := p.parseFile(path)
		if err != nil {
			return fmt.Errorf("parsing file %s: %w", path, err)
		}

		for _, doc := range docs {
			hr, ok := doc.(HelmRelease)
			if ok {
				result = append(result, hr)
			}
		}
		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("walking directory %s: %w", p.RootPath, err)
	}

	return result, nil
}

// ParseHelmRepositories discovers all Flux HelmRepository resources under the root path.
func (p *Parser) ParseHelmRepositories(ctx context.Context) ([]HelmRepository, error) {
	var result []HelmRepository

	err := filepath.WalkDir(p.RootPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() || !isYAMLFile(path) {
			return nil
		}

		docs, err := p.parseFile(path)
		if err != nil {
			return fmt.Errorf("parsing file %s: %w", path, err)
		}

		for _, doc := range docs {
			repo, ok := doc.(HelmRepository)
			if ok {
				result = append(result, repo)
			}
		}
		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("walking directory %s: %w", p.RootPath, err)
	}

	return result, nil
}

// ParseGitRepositories discovers all Flux GitRepository resources under the root path.
func (p *Parser) ParseGitRepositories(ctx context.Context) ([]GitRepository, error) {
	var result []GitRepository

	err := filepath.WalkDir(p.RootPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() || !isYAMLFile(path) {
			return nil
		}

		docs, err := p.parseFile(path)
		if err != nil {
			return fmt.Errorf("parsing file %s: %w", path, err)
		}

		for _, doc := range docs {
			repo, ok := doc.(GitRepository)
			if ok {
				result = append(result, repo)
			}
		}
		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("walking directory %s: %w", p.RootPath, err)
	}

	return result, nil
}

// parseFile reads a YAML file and splits it into individual documents,
// attempting to parse each one into a Flux resource type.
func (p *Parser) parseFile(path string) ([]interface{}, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading file: %w", err)
	}

	return parseYAMLDocuments(data)
}

// parseYAMLDocuments splits a multi-document YAML and parses each document.
func parseYAMLDocuments(data []byte) ([]interface{}, error) {
	var results []interface{}

	// Split on YAML document separator `---`
	docs := splitYAMLDocuments(data)

	for _, doc := range docs {
		trimmed := strings.TrimSpace(doc)
		if trimmed == "" {
			continue
		}

		parsed, err := parseSingleDocument([]byte(trimmed))
		if err != nil {
			// Skip documents we can't parse — they may not be Flux resources.
			continue
		}
		if parsed != nil {
			results = append(results, parsed)
		}
	}

	return results, nil
}

// parseSingleDocument parses a single YAML document into the appropriate Flux type.
func parseSingleDocument(data []byte) (interface{}, error) {
	// First, extract apiVersion and kind to determine the type.
	var meta struct {
		APIVersion string `yaml:"apiVersion"`
		Kind       string `yaml:"kind"`
	}

	if err := yaml.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("unmarshaling metadata: %w", err)
	}

	if meta.APIVersion == "" || meta.Kind == "" {
		return nil, nil
	}

	// Determine the resource type and parse accordingly.
	switch meta.Kind {
	case KindKustomization:
		if isKustomizeAPI(meta.APIVersion) {
			var ks Kustomization
			if err := yaml.Unmarshal(data, &ks); err != nil {
				return nil, fmt.Errorf("unmarshaling Kustomization: %w", err)
			}
			return ks, nil
		}
	case KindHelmRelease:
		if isHelmAPI(meta.APIVersion) {
			var hr HelmRelease
			if err := yaml.Unmarshal(data, &hr); err != nil {
				return nil, fmt.Errorf("unmarshaling HelmRelease: %w", err)
			}
			return hr, nil
		}
	case KindHelmRepository:
		if isSourceAPI(meta.APIVersion) {
			var repo HelmRepository
			if err := yaml.Unmarshal(data, &repo); err != nil {
				return nil, fmt.Errorf("unmarshaling HelmRepository: %w", err)
			}
			return repo, nil
		}
	case KindGitRepository:
		if isSourceAPI(meta.APIVersion) {
			var repo GitRepository
			if err := yaml.Unmarshal(data, &repo); err != nil {
				return nil, fmt.Errorf("unmarshaling GitRepository: %w", err)
			}
			return repo, nil
		}
	}

	return nil, nil
}

// splitYAMLDocuments splits a multi-document YAML into individual documents.
func splitYAMLDocuments(data []byte) []string {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var docs []string

	for {
		var node yaml.Node
		if err := decoder.Decode(&node); err != nil {
			break
		}
		var buf bytes.Buffer
		encoder := yaml.NewEncoder(&buf)
		encoder.SetIndent(2)
		if err := encoder.Encode(&node); err != nil {
			break
		}
		encoder.Close()
		docs = append(docs, buf.String())
	}

	return docs
}

// isKustomizeAPI checks if the apiVersion belongs to kustomize.toolkit.fluxcd.io.
func isKustomizeAPI(apiVersion string) bool {
	return strings.HasPrefix(apiVersion, GroupKustomizeToolkitFluxHelmIO)
}

// isHelmAPI checks if the apiVersion belongs to helm.toolkit.fluxcd.io.
func isHelmAPI(apiVersion string) bool {
	return strings.HasPrefix(apiVersion, GroupHelmToolkitFluxHelmIO)
}

// isSourceAPI checks if the apiVersion belongs to source.toolkit.fluxcd.io.
func isSourceAPI(apiVersion string) bool {
	return strings.HasPrefix(apiVersion, GroupSourceToolkitFluxHelmIO)
}

// isYAMLFile returns true if the file has a YAML extension.
func isYAMLFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".yaml" || ext == ".yml"
}
