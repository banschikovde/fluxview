package cli

import (
	"bytes"
	"fmt"
	"log"
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
	docs := splitYAMLText(data)
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
			log.Printf("Warning: skipping unparseable YAML document: %v", err)
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
			log.Printf("Warning: duplicate resource %s — overwriting previous entry", key)
			_ = existing
		}
		result[key] = processed
	}

	return result
}

// splitYAMLText splits multi-doc YAML by --- separators using plain text
// operations. Normalizes CRLF to LF first. Note: does not track block scalar
// context — a literal "---" inside a multiline block scalar (| or >) would
// be incorrectly treated as a separator. This is uncommon in kustomize output.
func splitYAMLText(data []byte) []string {
	normalized := strings.ReplaceAll(string(data), "\r\n", "\n")
	var docs []string
	for _, doc := range strings.Split(normalized, "\n---") {
		s := strings.TrimSpace(doc)
		s = strings.TrimPrefix(s, "---")
		s = strings.TrimSpace(s)
		if s != "" {
			docs = append(docs, s)
		}
	}
	return docs
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

// stripAttrsRecursive removes keys in attrs from m and all nested maps/slices.
// Used by stripAllAttrs for build path (not diff).
func stripAttrsRecursive(m map[string]interface{}, attrs map[string]bool) {
	for k := range m {
		if attrs[k] {
			delete(m, k)
		}
	}
	for _, val := range m {
		switch v := val.(type) {
		case map[string]interface{}:
			stripAttrsRecursive(v, attrs)
		case []interface{}:
			for _, item := range v {
				if nested, ok := item.(map[string]interface{}); ok {
					stripAttrsRecursive(nested, attrs)
				}
			}
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
	docs := splitYAMLText(data)
	var result []string
	for _, doc := range docs {
		var meta struct {
			Kind string `yaml:"kind"`
		}
		if err := yaml.Unmarshal([]byte(doc), &meta); err != nil {
			log.Printf("Warning: skipping unparseable document in filterCRDDocs: %v", err)
			continue
		}
		if meta.Kind != "CustomResourceDefinition" {
			result = append(result, doc)
		}
	}
	return []byte(strings.Join(result, "\n---\n"))
}

// filterByNamespace keeps only YAML documents whose metadata.namespace matches.
func filterByNamespace(data []byte, namespace string) []byte {
	if namespace == "" {
		return data
	}
	docs := splitYAMLText(data)
	var result []string
	for _, doc := range docs {
		var meta struct {
			Metadata struct {
				Namespace string `yaml:"namespace"`
			} `yaml:"metadata"`
		}
		if err := yaml.Unmarshal([]byte(doc), &meta); err != nil {
			log.Printf("Warning: skipping unparseable document in filterByNamespace: %v", err)
			continue
		}
		if meta.Metadata.Namespace == namespace {
			result = append(result, doc)
		}
	}
	return []byte(strings.Join(result, "\n---\n"))
}

// stripAllAttrs strips specified keys from all documents in multi-doc YAML.
func stripAllAttrs(data []byte, attrsList string) []byte {
	attrs := parseAttrs(attrsList)
	if attrs == nil {
		return data
	}
	docs := splitYAMLText(data)
	var result []string
	for _, doc := range docs {
		result = append(result, stripAttrsFromDoc(doc, attrs))
	}
	return []byte(strings.Join(result, "\n---\n"))
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
