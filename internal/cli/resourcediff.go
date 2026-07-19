package cli

import (
	"bytes"
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	diffpkg "github.com/banschikovde/fluxview/internal/diff"
	"github.com/banschikovde/fluxview/internal/flux"
)

// resourceKey identifies a Kubernetes resource by kind/namespace/name.
type resourceKey struct {
	Kind      string
	Namespace string
	Name      string
}

func (k resourceKey) String() string {
	if k.Namespace != "" {
		return fmt.Sprintf("%s: %s/%s", k.Kind, k.Namespace, k.Name)
	}
	return fmt.Sprintf("%s: %s", k.Kind, k.Name)
}

// resourceDiffResult holds a diff for a single resource.
type resourceDiffResult struct {
	Key     resourceKey
	Status  string // "modified", "added", "removed"
	RawDiff string
}

// buildResourceMap splits multi-doc YAML into individual resources in a single
// pass, applying redaction, attribute stripping, and CRD filtering. This
// replaces the previous multi-step pipeline that parsed YAML 4+ times.
func buildResourceMap(data []byte, flags *DiffFlags) map[resourceKey]string {
	stripAttrs := parseAttrs(flags.StripAttrs)
	docs := flux.SplitYAMLText(data)
	result := make(map[resourceKey]string)

	for _, doc := range docs {
		trimmed := strings.TrimSpace(doc)
		if trimmed == "" {
			continue
		}

		// Parse metadata only (fast — small struct).
		var meta struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name      string `yaml:"name"`
				Namespace string `yaml:"namespace"`
			} `yaml:"metadata"`
		}
		if err := yaml.Unmarshal([]byte(trimmed), &meta); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: skipping unparseable YAML document: %v\n", err)
			continue
		}
		if meta.Kind == "" || meta.Metadata.Name == "" {
			continue
		}

		// Skip CRDs if requested.
		if flags.SkipCRDs && meta.Kind == "CustomResourceDefinition" {
			continue
		}

		processed := trimmed

		// Strip specified attrs if requested.
		if len(stripAttrs) > 0 {
			processed = stripAttrsFromDoc(processed, stripAttrs)
		}

		// Redact secrets (only for Secret kind).
		if strings.EqualFold(meta.Kind, "secret") {
			processed = string(flux.RedactSecrets([]byte(processed)))
		}

		key := resourceKey{
			Kind:      meta.Kind,
			Namespace: meta.Metadata.Namespace,
			Name:      meta.Metadata.Name,
		}
		if existing, exists := result[key]; exists {
			// Only warn for genuinely conflicting resources.
			// Kustomization duplicates are expected from recursive discovery
			// (KS YAML prepended + same KS in kustomize output).
			if existing != processed && meta.Kind != "Kustomization" {
				fmt.Fprintf(os.Stderr, "Warning: duplicate resource %s — overwriting with different content\n", key)
			}
		}
		result[key] = processed
	}

	return result
}

// stripAttrsFromDoc removes the specified keys recursively from a YAML document.
// Uses yaml.Node to preserve original formatting, comments, key ordering, and
// scalar styles (quotes, block scalars).
func stripAttrsFromDoc(doc string, attrs map[string]bool) string {
	if len(attrs) == 0 {
		return doc
	}
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(doc), &node); err != nil {
		return doc
	}
	stripAttrsNode(&node, attrs)
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&node); err != nil {
		return doc
	}
	enc.Close()
	return strings.TrimSpace(buf.String())
}

// stripAttrsNode recursively removes keys in attrs from a yaml.Node tree.
func stripAttrsNode(node *yaml.Node, attrs map[string]bool) {
	if node == nil {
		return
	}
	switch node.Kind {
	case yaml.DocumentNode:
		for _, child := range node.Content {
			stripAttrsNode(child, attrs)
		}
	case yaml.MappingNode:
		var kept []*yaml.Node
		for i := 0; i+1 < len(node.Content); i += 2 {
			keyNode := node.Content[i]
			valNode := node.Content[i+1]
			if attrs[keyNode.Value] {
				continue
			}
			kept = append(kept, keyNode, valNode)
			stripAttrsNode(valNode, attrs)
		}
		node.Content = kept
	case yaml.SequenceNode:
		for _, child := range node.Content {
			stripAttrsNode(child, attrs)
		}
	}
}

// parseAttrs parses a comma-separated string into a set of keys.
func parseAttrs(s string) map[string]bool {
	if s == "" {
		return nil
	}
	attrs := make(map[string]bool)
	for _, key := range strings.Split(s, ",") {
		key = strings.TrimSpace(key)
		if key != "" {
			attrs[key] = true
		}
	}
	return attrs
}

// filterCRDDocs removes CustomResourceDefinition documents from multi-doc YAML.
func filterCRDDocs(data []byte) []byte {
	docs := flux.SplitYAMLText(data)
	var result []string
	for _, doc := range docs {
		var meta struct {
			Kind string `yaml:"kind"`
		}
		if err := yaml.Unmarshal([]byte(doc), &meta); err != nil {
			result = append(result, doc) // keep unparseable docs — don't lose data
			continue
		}
		if meta.Kind != "CustomResourceDefinition" {
			result = append(result, doc)
		}
	}
	return []byte(strings.Join(result, "\n---\n"))
}

// filterByNamespace keeps only YAML documents whose metadata.namespace matches.
// Special cases:
//   - kind: Namespace with metadata.name == namespace (cluster-scoped, matches by name)
//   - namespace == "default": resources with empty metadata.namespace match
//     (except cluster-scoped kinds like Namespace, ClusterRole, etc.)
func filterByNamespace(data []byte, namespace string) []byte {
	if namespace == "" {
		return data
	}
	docs := flux.SplitYAMLText(data)
	var result []string
	for _, doc := range docs {
		var meta struct {
			APIVersion string `yaml:"apiVersion"`
			Kind       string `yaml:"kind"`
			Metadata   struct {
				Name      string `yaml:"name"`
				Namespace string `yaml:"namespace"`
			} `yaml:"metadata"`
		}
		if err := yaml.Unmarshal([]byte(doc), &meta); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: skipping unparseable document in filterByNamespace: %v\n", err)
			continue
		}

		// Namespace resources (v1): match by metadata.name.
		if meta.APIVersion == "v1" && meta.Kind == "Namespace" && meta.Metadata.Name == namespace {
			result = append(result, doc)
			continue
		}

		// Exact namespace match.
		if meta.Metadata.Namespace == namespace {
			result = append(result, doc)
			continue
		}

		// Default namespace: resources with empty namespace match
		// (except cluster-scoped kinds).
		if namespace == "default" && meta.Metadata.Namespace == "" && !isClusterScoped(meta.Kind) {
			result = append(result, doc)
		}
	}
	return []byte(strings.Join(result, "\n---\n"))
}

// isClusterScoped returns true for Kubernetes kinds that are not namespaced.
var clusterScopedKinds = map[string]bool{
	"Namespace":                      true,
	"Node":                           true,
	"PersistentVolume":               true,
	"ClusterRole":                    true,
	"ClusterRoleBinding":             true,
	"CustomResourceDefinition":       true,
	"StorageClass":                   true,
	"PriorityClass":                  true,
	"ValidatingWebhookConfiguration": true,
	"MutatingWebhookConfiguration":   true,
	"APIService":                     true,
	"PodSecurityPolicy":              true,
	"RuntimeClass":                   true,
	"IngressClass":                   true,
	"VolumeAttachment":               true,
	"CertificateSigningRequest":      true,
	"TokenReview":                    true,
	"SelfSubjectAccessReview":        true,
	"SubjectAccessReview":            true,
}

func isClusterScoped(kind string) bool {
	return clusterScopedKinds[kind]
}

// stripAllAttrs strips specified keys from all documents in multi-doc YAML.
func stripAllAttrs(data []byte, attrsList string) []byte {
	attrs := parseAttrs(attrsList)
	if attrs == nil {
		return data
	}
	docs := flux.SplitYAMLText(data)
	var result []string
	for _, doc := range docs {
		result = append(result, stripAttrsFromDoc(doc, attrs))
	}
	return []byte(strings.Join(result, "\n---\n"))
}

// injectNamespace adds metadata.namespace to every namespaced resource in
// data that lacks one, mirroring what `kubectl apply -n <ns>` and
// `helm install --namespace <ns>` do at apply time: a resource without an
// explicit metadata.namespace is assigned to the request namespace.
//
// This is needed because `helm template --namespace X` only sets the
// .Release.Namespace template variable — it does NOT inject
// metadata.namespace into the rendered output. Charts that don't template
// {{ .Release.Namespace }} produce resources without an explicit namespace,
// which then breaks downstream resource-level filtering (diff hr --namespace).
//
// Documents are parsed with yaml.Node so formatting, comments, key ordering,
// and scalar styles are preserved. Resources that already declare a
// namespace, cluster-scoped kinds (Namespace, ClusterRole, CRD, ...), and
// documents without apiVersion/kind are returned unchanged.
func injectNamespace(data []byte, namespace string) []byte {
	if namespace == "" {
		return data
	}
	docs := flux.SplitYAMLText(data)
	var result []string
	for _, doc := range docs {
		result = append(result, injectNamespaceDoc(doc, namespace))
	}
	return []byte(strings.Join(result, "\n---\n"))
}

// injectNamespaceDoc adds metadata.namespace to a single YAML document if
// it represents a namespaced Kubernetes resource that lacks an explicit
// namespace. See injectNamespace for the full contract.
func injectNamespaceDoc(doc, namespace string) string {
	if namespace == "" {
		return doc
	}
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(doc), &node); err != nil {
		return doc // keep unparseable docs unchanged
	}
	mapping := topLevelMapping(&node)
	if mapping == nil {
		return doc
	}
	// A Kubernetes resource must declare both apiVersion and kind. Helm
	// output always has both, but defensively skip partial documents
	// rather than fabricating metadata on something that isn't a resource.
	apiVersion := mapScalarValue(mapping, "apiVersion")
	kind := mapScalarValue(mapping, "kind")
	if apiVersion == "" || kind == "" || isClusterScoped(kind) {
		return doc
	}
	metadataIdx := mappingKeyIndex(mapping, "metadata")
	if metadataIdx == -1 {
		return doc // no metadata → don't fabricate one
	}
	metadataNode := mapping.Content[metadataIdx+1]
	if metadataNode.Kind != yaml.MappingNode {
		return doc
	}
	if mappingKeyIndex(metadataNode, "namespace") != -1 {
		return doc // explicit namespace already set — never override
	}
	// Insert "namespace" immediately after "name" when present (k8s
	// convention: name, namespace, labels, annotations, ...); otherwise
	// prepend at the start of the metadata mapping.
	insertIdx := 0
	if nameIdx := mappingKeyIndex(metadataNode, "name"); nameIdx != -1 {
		insertIdx = nameIdx + 2 // after the [name, value] pair
	}
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "namespace"}
	valNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: namespace}
	metadataNode.Content = append(
		metadataNode.Content[:insertIdx],
		append([]*yaml.Node{keyNode, valNode}, metadataNode.Content[insertIdx:]...)...,
	)
	// Use the same indent as ConvertJSONInYAMLToYAML (yaml.Marshal default,
	// 4 spaces) so injected docs match the indent of docs that already had
	// a namespace and weren't re-encoded. Mixing indents in one diff output
	// would be visible noise.
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(4)
	if err := enc.Encode(&node); err != nil {
		return doc
	}
	enc.Close()
	return strings.TrimSpace(buf.String())
}

// topLevelMapping returns the top-level mapping node of a parsed YAML
// document, or nil if the document is empty or not a mapping.
func topLevelMapping(node *yaml.Node) *yaml.Node {
	if node == nil || node.Kind != yaml.DocumentNode || len(node.Content) == 0 {
		return nil
	}
	if node.Content[0].Kind != yaml.MappingNode {
		return nil
	}
	return node.Content[0]
}

// mapScalarValue returns the string value of a scalar mapping key, or "" if
// the key is absent or holds a non-scalar value.
func mapScalarValue(mapping *yaml.Node, key string) string {
	idx := mappingKeyIndex(mapping, key)
	if idx == -1 {
		return ""
	}
	v := mapping.Content[idx+1]
	if v == nil || v.Kind != yaml.ScalarNode {
		return ""
	}
	return v.Value
}

// mappingKeyIndex returns the index of the key node named key in a mapping
// node's Content slice (which alternates key, value, key, value, ...), or
// -1 if not found.
func mappingKeyIndex(mapping *yaml.Node, key string) int {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return -1
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if k := mapping.Content[i]; k != nil && k.Kind == yaml.ScalarNode && k.Value == key {
			return i
		}
	}
	return -1
}

// diffResourceMaps matches resources by key and computes per-resource diffs.
// Identical resources (same text) are skipped without running LCS.
func diffResourceMaps(origMap, modMap map[resourceKey]string, ctxLines int) []resourceDiffResult {
	// Collect all keys.
	allKeys := make(map[resourceKey]bool)
	for k := range origMap {
		allKeys[k] = true
	}
	for k := range modMap {
		allKeys[k] = true
	}

	var results []resourceDiffResult
	for key := range allKeys {
		orig, inOrig := origMap[key]
		mod, inMod := modMap[key]

		switch {
		case inOrig && inMod:
			// Fast path: skip identical resources without LCS.
			if orig == mod {
				continue
			}
			result := diffpkg.ComputeCtx(orig, mod, ctxLines)
			if result.HasDiff {
				results = append(results, resourceDiffResult{
					Key:     key,
					Status:  "modified",
					RawDiff: result.RawDiff,
				})
			}
		case inMod:
			results = append(results, resourceDiffResult{
				Key:     key,
				Status:  "added",
				RawDiff: mod,
			})
		case inOrig:
			results = append(results, resourceDiffResult{
				Key:     key,
				Status:  "removed",
				RawDiff: orig,
			})
		}
	}

	// Sort for stable output: by kind, namespace, name.
	sort.Slice(results, func(i, j int) bool {
		a, b := results[i].Key, results[j].Key
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})

	return results
}

// formatResourceDiffs formats per-resource diffs with box headers (flux-local style).
func formatResourceDiffs(diffs []resourceDiffResult, useColor bool) string {
	var buf strings.Builder

	for _, d := range diffs {
		header := d.Key.String()
		if d.Status != "modified" {
			header += fmt.Sprintf(" (%s)", d.Status)
		}
		border := strings.Repeat("-", len(header)+2)
		buf.WriteString(border + "\n")
		buf.WriteString(" " + header + "\n")
		buf.WriteString(border + "\n")

		switch d.Status {
		case "modified":
			if useColor {
				buf.WriteString(diffpkg.Colorize(d.RawDiff))
			} else {
				buf.WriteString(d.RawDiff)
			}
		case "added":
			for _, line := range strings.Split(strings.TrimSpace(d.RawDiff), "\n") {
				if line == "" {
					continue
				}
				if useColor {
					buf.WriteString(diffpkg.ANSIGreen + line + diffpkg.ANSIReset + "\n")
				} else {
					buf.WriteString("+ " + line + "\n")
				}
			}
		case "removed":
			for _, line := range strings.Split(strings.TrimSpace(d.RawDiff), "\n") {
				if line == "" {
					continue
				}
				if useColor {
					buf.WriteString(diffpkg.ANSIRed + line + diffpkg.ANSIReset + "\n")
				} else {
					buf.WriteString("- " + line + "\n")
				}
			}
		}
		buf.WriteString("\n")
	}

	return buf.String()
}
