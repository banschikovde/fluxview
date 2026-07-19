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

	"github.com/banschikovde/fluxview/internal/yamlutil"
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

// isChartRoot reports whether dir is the root of a Helm chart.
//
// A Helm chart root is identified by the presence of a Chart.yaml file next
// to it. Chart subtrees contain Go-template text under templates/ (and other
// chart-only files such as values.yaml) that is not standalone YAML and must
// not be scanned by raw resource parsers: SplitYAMLDocuments cannot render
// Go templates and would emit spurious "YAML parse error" warnings for them.
func isChartRoot(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, "Chart.yaml"))
	if err != nil || info.IsDir() {
		return false
	}
	return true
}

// walkYAMLFiles walks rootPath, skipping the entire subtree of any directory
// that is a Helm chart root, and invokes fn for every YAML file found.
//
// Skipping chart roots (via filepath.SkipDir) prevents the parser from
// descending into templates/ and trying to decode Go-template files as YAML.
// fn receives the file path and may return an error to abort the walk
// (filepath.SkipDir skips just the current file). The walk honors ctx
// cancellation.
func walkYAMLFiles(ctx context.Context, rootPath string, fn func(path string) error) error {
	return filepath.WalkDir(rootPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if isChartRoot(path) {
				return filepath.SkipDir
			}
			return nil
		}
		if !IsYAMLFile(path) {
			return nil
		}
		return fn(path)
	})
}

// ParseKustomizations discovers all Flux Kustomization resources under the root path.
func (p *Parser) ParseKustomizations(ctx context.Context) ([]Kustomization, error) {
	var result []Kustomization
	var yamlFiles int
	var parseErrors []string

	err := walkYAMLFiles(ctx, p.RootPath, func(path string) error {
		yamlFiles++

		docs, err := p.parseFile(path)
		if err != nil {
			parseErrors = append(parseErrors, fmt.Sprintf("%s: %v", path, err))
			return nil
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

	if len(result) == 0 {
		msg := fmt.Sprintf("no Flux Kustomization resources found in %s (scanned %d YAML files)", p.RootPath, yamlFiles)
		if len(parseErrors) > 0 {
			msg += fmt.Sprintf(", %d parse errors: %s", len(parseErrors), strings.Join(parseErrors, "; "))
		}
		return nil, fmt.Errorf("%s", msg)
	}

	return result, nil
}

// ParseHelmRepositories discovers all Flux HelmRepository resources under the root path.
func (p *Parser) ParseHelmRepositories(ctx context.Context) ([]HelmRepository, error) {
	var result []HelmRepository

	err := walkYAMLFiles(ctx, p.RootPath, func(path string) error {
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

// ParseOCIRepositories discovers all Flux OCIRepository resources under the root path.
func (p *Parser) ParseOCIRepositories(ctx context.Context) ([]OCIRepository, error) {
	var result []OCIRepository

	err := walkYAMLFiles(ctx, p.RootPath, func(path string) error {
		docs, err := p.parseFile(path)
		if err != nil {
			return fmt.Errorf("parsing file %s: %w", path, err)
		}

		for _, doc := range docs {
			repo, ok := doc.(OCIRepository)
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

// ParseConfigMaps discovers all Kubernetes ConfigMap resources under the root path.
func (p *Parser) ParseConfigMaps(ctx context.Context) ([]ConfigMap, error) {
	var result []ConfigMap

	err := walkYAMLFiles(ctx, p.RootPath, func(path string) error {
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not read %s: %v\n", path, err)
			return nil
		}

		docs := SplitYAMLDocuments(data)
		for _, doc := range docs {
			trimmed := strings.TrimSpace(doc)
			if trimmed == "" {
				continue
			}
			cm, err := parseConfigMapDoc([]byte(trimmed))
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: could not parse ConfigMap document in %s: %v\n", path, err)
				continue
			}
			if cm != nil {
				result = append(result, *cm)
			}
		}
		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("walking directory %s: %w", p.RootPath, err)
	}

	return result, nil
}

// ParseSecrets discovers all Kubernetes Secret resources under the root path.
func (p *Parser) ParseSecrets(ctx context.Context) ([]Secret, error) {
	var result []Secret

	err := walkYAMLFiles(ctx, p.RootPath, func(path string) error {
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not read %s: %v\n", path, err)
			return nil
		}

		docs := SplitYAMLDocuments(data)
		for _, doc := range docs {
			trimmed := strings.TrimSpace(doc)
			if trimmed == "" {
				continue
			}
			secret, err := parseSecretDoc([]byte(trimmed))
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: could not parse Secret document in %s: %v\n", path, err)
				continue
			}
			if secret != nil {
				result = append(result, *secret)
			}
		}
		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("walking directory %s: %w", p.RootPath, err)
	}

	return result, nil
}

// parseConfigMapDoc parses a single YAML document as a ConfigMap.
// The document is parsed into a yaml.Node once; apiVersion/kind are read from
// the node to dispatch, then the same node is decoded into the target — this
// avoids a second byte-for-byte parse of the document.
func parseConfigMapDoc(data []byte) (*ConfigMap, error) {
	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil {
		return nil, err
	}
	mapping := mappingFor(&node)
	if mapping == nil {
		return nil, nil
	}
	if mapScalar(mapping, "apiVersion") != "v1" || mapScalar(mapping, "kind") != "ConfigMap" {
		return nil, nil
	}
	var cm ConfigMap
	if err := node.Decode(&cm); err != nil {
		return nil, err
	}
	return &cm, nil
}

// parseSecretDoc parses a single YAML document as a Secret (single-parse, see parseConfigMapDoc).
func parseSecretDoc(data []byte) (*Secret, error) {
	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil {
		return nil, err
	}
	mapping := mappingFor(&node)
	if mapping == nil {
		return nil, nil
	}
	if mapScalar(mapping, "apiVersion") != "v1" || mapScalar(mapping, "kind") != "Secret" {
		return nil, nil
	}
	var secret Secret
	if err := node.Decode(&secret); err != nil {
		return nil, err
	}
	return &secret, nil
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
	docs := SplitYAMLDocuments(data)

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
// The document is parsed into a yaml.Node once; apiVersion/kind are read from the
// node to dispatch, then the same node is decoded into the target type — this
// avoids a second byte-for-byte parse of the document.
func parseSingleDocument(data []byte) (interface{}, error) {
	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil {
		return nil, fmt.Errorf("unmarshaling metadata: %w", err)
	}
	mapping := mappingFor(&node)
	if mapping == nil {
		return nil, nil
	}
	apiVersion := mapScalar(mapping, "apiVersion")
	kind := mapScalar(mapping, "kind")
	if apiVersion == "" || kind == "" {
		return nil, nil
	}

	// Determine the resource type and decode the same node into it.
	switch kind {
	case KindKustomization:
		if isKustomizeAPI(apiVersion) {
			var ks Kustomization
			if err := node.Decode(&ks); err != nil {
				return nil, fmt.Errorf("unmarshaling Kustomization: %w", err)
			}
			return ks, nil
		}
	case KindHelmRepository:
		if isSourceAPI(apiVersion) {
			var repo HelmRepository
			if err := node.Decode(&repo); err != nil {
				return nil, fmt.Errorf("unmarshaling HelmRepository: %w", err)
			}
			return repo, nil
		}
	case KindOCIRepository:
		if isSourceAPI(apiVersion) {
			var repo OCIRepository
			if err := node.Decode(&repo); err != nil {
				return nil, fmt.Errorf("unmarshaling OCIRepository: %w", err)
			}
			return repo, nil
		}
	}

	return nil, nil
}

// mappingFor returns the top-level mapping node of a parsed YAML document, or
// nil if the document is empty or not a mapping.
func mappingFor(node *yaml.Node) *yaml.Node {
	if node == nil || node.Kind != yaml.DocumentNode || len(node.Content) == 0 {
		return nil
	}
	if node.Content[0].Kind != yaml.MappingNode {
		return nil
	}
	return node.Content[0]
}

// mapScalar returns the string value of a scalar mapping key, or "" if the key
// is absent or holds a non-scalar value.
func mapScalar(mapping *yaml.Node, key string) string {
	v := getMapValue(mapping, key)
	if v == nil {
		return ""
	}
	return v.Value
}

// SplitYAMLDocuments splits a multi-document YAML into individual documents.
func SplitYAMLDocuments(data []byte) []string {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var docs []string

	for {
		var node yaml.Node
		if err := decoder.Decode(&node); err != nil {
			if err != io.EOF {
				fmt.Fprintf(os.Stderr, "Warning: YAML parse error in SplitYAMLDocuments: %v\n", err)
			}
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

// SplitYAMLText splits multi-doc YAML into individual documents.
// See internal/yamlutil.SplitYAMLText for the implementation; this is a
// thin wrapper kept so existing callers in the flux package and its users
// don't depend on yamlutil directly.
func SplitYAMLText(data []byte) []string {
	return yamlutil.SplitYAMLText(data)
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

// IsYAMLFile returns true if the file has a YAML extension.
func IsYAMLFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".yaml" || ext == ".yml"
}
