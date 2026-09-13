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
//
// The first ParseXxx call performs one tree walk and caches a
// ResourceSnapshot covering every supported resource type; later ParseXxx
// calls on the same Parser filter that snapshot instead of re-walking (and
// re-reading + re-parsing) the tree. Share one Parser per root path within
// a command to pay for exactly one walk. A Parser is not safe for
// concurrent use.
type Parser struct {
	// RootPath is the root path of the git repository or cluster directory.
	RootPath string

	snapshot *ResourceSnapshot
	snapErr  error
}

// NewParser creates a new Parser rooted at the given path.
func NewParser(rootPath string) *Parser {
	return &Parser{RootPath: rootPath}
}

// snapshotOf returns the cached all-types walk result, walking the root
// lazily on the first call.
func (p *Parser) snapshotOf(ctx context.Context) (*ResourceSnapshot, error) {
	if p.snapshot == nil && p.snapErr == nil {
		p.snapshot, p.snapErr = WalkResources(ctx, p.RootPath)
	}
	return p.snapshot, p.snapErr
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

// ResourceSnapshot holds every resource type discovered by a single tree
// walk, so callers needing several types pay for one walk — and one read +
// parse per file — instead of one walk per type.
type ResourceSnapshot struct {
	// Kustomizations are Flux Kustomization resources.
	Kustomizations []Kustomization
	// HelmReleases are Flux HelmRelease resources.
	HelmReleases []HelmRelease
	// HelmRepositories are Flux HelmRepository resources.
	HelmRepositories []HelmRepository
	// OCIRepositories are Flux OCIRepository resources.
	OCIRepositories []OCIRepository
	// ConfigMaps are plain v1 ConfigMap resources.
	ConfigMaps []ConfigMap
	// Secrets are plain v1 Secret resources.
	Secrets []Secret
	// YAMLFilesScanned is the number of YAML files the walk visited.
	YAMLFilesScanned int
	// ReadErrors lists per-file read failures as "<path>: reading file: <err>",
	// used to compose the ParseKustomizations "no resources found" message.
	ReadErrors []string
}

// WalkResources walks rootPath once (skipping Helm chart subtrees), reads
// each YAML file once, and dispatches every document by (apiVersion, kind)
// into the returned snapshot.
//
// Failure handling mirrors the historic per-type parsers: a file that cannot
// be read is warned on stderr and skipped (the walk continues); a v1
// ConfigMap/Secret document that fails to decode is warned and skipped; other
// unparseable documents are skipped silently. The returned error (wrapped as
// "walking directory <root>") aborts the whole walk — a cancelled context or
// an inaccessible root.
func WalkResources(ctx context.Context, rootPath string) (*ResourceSnapshot, error) {
	snap := &ResourceSnapshot{}
	err := walkYAMLFiles(ctx, rootPath, func(path string) error {
		snap.YAMLFilesScanned++

		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not read %s: %v\n", path, err)
			snap.ReadErrors = append(snap.ReadErrors, fmt.Sprintf("%s: %v", path, fmt.Errorf("reading file: %w", err)))
			return nil
		}

		decoder := yaml.NewDecoder(bytes.NewReader(data))
		for {
			var node yaml.Node
			if err := decoder.Decode(&node); err != nil {
				if err != io.EOF {
					fmt.Fprintf(os.Stderr, "Warning: YAML parse error in %s: %v\n", path, err)
				}
				return nil
			}
			snap.dispatch(path, &node)
		}
	})
	if err != nil {
		return nil, fmt.Errorf("walking directory %s: %w", rootPath, err)
	}
	return snap, nil
}

// dispatch decodes one document node into the snapshot by (apiVersion, kind).
// The node has been parsed once by the caller; the target-type decode reuses
// it, so each document is parsed exactly once no matter how many types the
// snapshot carries.
func (s *ResourceSnapshot) dispatch(path string, node *yaml.Node) {
	mapping := mappingFor(node)
	if mapping == nil {
		return
	}
	apiVersion := mapScalar(mapping, "apiVersion")
	kind := mapScalar(mapping, "kind")
	if apiVersion == "" || kind == "" {
		return
	}

	switch {
	case kind == KindKustomization && isKustomizeAPI(apiVersion):
		var ks Kustomization
		if err := node.Decode(&ks); err == nil {
			s.Kustomizations = append(s.Kustomizations, ks)
		}
	case kind == KindHelmRelease && isHelmAPI(apiVersion):
		var hr HelmRelease
		if err := node.Decode(&hr); err == nil {
			s.HelmReleases = append(s.HelmReleases, hr)
		}
	case kind == KindHelmRepository && isSourceAPI(apiVersion):
		var repo HelmRepository
		if err := node.Decode(&repo); err == nil {
			s.HelmRepositories = append(s.HelmRepositories, repo)
		}
	case kind == KindOCIRepository && isSourceAPI(apiVersion):
		var repo OCIRepository
		if err := node.Decode(&repo); err == nil {
			s.OCIRepositories = append(s.OCIRepositories, repo)
		}
	case kind == "ConfigMap" && apiVersion == "v1":
		var cm ConfigMap
		if err := node.Decode(&cm); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not parse ConfigMap document in %s: %v\n", path, err)
		} else {
			s.ConfigMaps = append(s.ConfigMaps, cm)
		}
	case kind == "Secret" && apiVersion == "v1":
		var secret Secret
		if err := node.Decode(&secret); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not parse Secret document in %s: %v\n", path, err)
		} else {
			s.Secrets = append(s.Secrets, secret)
		}
	}
}

// ParseKustomizations discovers all Flux Kustomization resources under the root path.
func (p *Parser) ParseKustomizations(ctx context.Context) ([]Kustomization, error) {
	snap, err := p.snapshotOf(ctx)
	if err != nil {
		return nil, err
	}

	if len(snap.Kustomizations) == 0 {
		msg := fmt.Sprintf("no Flux Kustomization resources found in %s (scanned %d YAML files)", p.RootPath, snap.YAMLFilesScanned)
		if len(snap.ReadErrors) > 0 {
			msg += fmt.Sprintf(", %d parse errors: %s", len(snap.ReadErrors), strings.Join(snap.ReadErrors, "; "))
		}
		return nil, fmt.Errorf("%s", msg)
	}

	return snap.Kustomizations, nil
}

// ParseHelmReleases discovers all Flux HelmRelease resources under the root path.
func (p *Parser) ParseHelmReleases(ctx context.Context) ([]HelmRelease, error) {
	snap, err := p.snapshotOf(ctx)
	if err != nil {
		return nil, err
	}
	return snap.HelmReleases, nil
}

// ParseHelmRepositories discovers all Flux HelmRepository resources under the root path.
func (p *Parser) ParseHelmRepositories(ctx context.Context) ([]HelmRepository, error) {
	snap, err := p.snapshotOf(ctx)
	if err != nil {
		return nil, err
	}
	return snap.HelmRepositories, nil
}

// ParseOCIRepositories discovers all Flux OCIRepository resources under the root path.
func (p *Parser) ParseOCIRepositories(ctx context.Context) ([]OCIRepository, error) {
	snap, err := p.snapshotOf(ctx)
	if err != nil {
		return nil, err
	}
	return snap.OCIRepositories, nil
}

// ParseConfigMaps discovers all Kubernetes ConfigMap resources under the root path.
func (p *Parser) ParseConfigMaps(ctx context.Context) ([]ConfigMap, error) {
	snap, err := p.snapshotOf(ctx)
	if err != nil {
		return nil, err
	}
	return snap.ConfigMaps, nil
}

// ParseSecrets discovers all Kubernetes Secret resources under the root path.
func (p *Parser) ParseSecrets(ctx context.Context) ([]Secret, error) {
	snap, err := p.snapshotOf(ctx)
	if err != nil {
		return nil, err
	}
	return snap.Secrets, nil
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
