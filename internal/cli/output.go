package cli

import (
	"bytes"
	"cmp"
	"fmt"
	"os"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/banschikovde/fluxview/internal/flux"
	"github.com/banschikovde/fluxview/internal/helm"
	"github.com/banschikovde/fluxview/internal/yamlutil"
)

// resourceEntry holds a single YAML document with its resource key for sorting.
type resourceEntry struct {
	key     resourceKey
	content string
}

// outputOptions configures the single-pass processing of build output: every
// step used to be its own full re-parse of the combined output (filter →
// filter → strip → print).
type outputOptions struct {
	// namespace keeps only resources in this namespace ("" disables).
	namespace string
	// skipCRDs drops CustomResourceDefinition documents.
	skipCRDs bool
	// stripAttrs lists keys removed recursively from every document.
	stripAttrs map[string]bool
}

// processResources splits multi-doc YAML into individual resources in a
// single pass, applying the namespace filter, CRD filter, attribute
// stripping, field reordering, JSON-in-YAML conversion, and secret redaction
// per document, then sorts entries by kind/namespace/name.
//
// Each document is parsed into a yaml.Node exactly once; every transform
// (strip, SOPS removal, key reorder) mutates that node, and the
// JSON-in-YAML conversion decodes straight from it — the former chain
// re-encoded and re-parsed the text between each stage (4 parses and 3
// encodes per document). The final bytes still come from the same
// marshal+redact tail the old chain ended with.
func processResources(data []byte, opts outputOptions) []resourceEntry {
	var entries []resourceEntry

	for _, doc := range flux.SplitYAMLText(data) {
		trimmed := strings.TrimSpace(doc)
		if trimmed == "" {
			continue
		}

		var meta docMeta
		// meta is intentionally parsed BEFORE stripping: --strip-attrs targets
		// noise fields (status, creationTimestamp, helm.sh/chart), and stripping
		// the identity fields (kind/name/namespace) is outside the contract.
		if err := yaml.Unmarshal([]byte(trimmed), &meta); err != nil {
			if opts.namespace != "" {
				// Parity with filterByNamespace: warn about unparseable
				// documents only while namespace-filtering.
				fmt.Fprintf(os.Stderr, "Warning: skipping unparseable document in namespace filter: %v\n", err)
			}
			continue
		}

		if opts.namespace != "" && !meta.matchesNamespace(opts.namespace) {
			continue
		}

		// Skip CRDs if requested.
		if opts.skipCRDs && meta.Kind == "CustomResourceDefinition" {
			continue
		}

		if meta.Kind == "" || meta.Metadata.Name == "" {
			continue
		}

		var node yaml.Node
		if err := yaml.Unmarshal([]byte(trimmed), &node); err != nil {
			// Parity with the old chain: a document the generic parser
			// rejects was dropped by ConvertJSONInYAMLToYAML.
			continue
		}
		mapping := mappingNode(&node)
		if mapping == nil {
			continue
		}

		if len(opts.stripAttrs) > 0 {
			stripAttrsNode(&node, opts.stripAttrs)
		}
		removeMapKey(mapping, "sops")
		reorderMapKeys(mapping, []string{"apiVersion", "kind", "metadata"})

		var content string
		if !yamlutil.NodeNeedsConversion(&node) {
			// Clean document — no JSON flow style, no nil values: the
			// transforms above are all it needed, so encode the node once
			// and keep its key order. Only Secrets pay the extra redaction
			// round-trip (same as before).
			out := encodeNode(&node)
			if strings.EqualFold(meta.Kind, "secret") {
				out = flux.RedactSecrets(out)
			}
			content = strings.TrimSpace(string(out))
		} else {
			// JSON-in-YAML conversion without the text round-trip: decode
			// the transformed node into a generic value (this is what
			// helm.ConvertJSONInYAMLToYAML re-parsed from text), drop nils
			// and marshal — the alphabetical-key marshal IS the conversion
			// output.
			var v interface{}
			if err := node.Decode(&v); err != nil || v == nil {
				continue
			}
			marshaled, err := yaml.Marshal(helm.RemoveNilValues(v))
			if err != nil {
				continue
			}
			content = strings.TrimSpace(string(flux.RedactSecrets(marshaled)))
		}

		entries = append(entries, resourceEntry{
			key: resourceKey{
				Kind:      meta.Kind,
				Namespace: meta.Metadata.Namespace,
				Name:      meta.Metadata.Name,
			},
			content: content,
		})
	}

	slices.SortFunc(entries, func(a, b resourceEntry) int {
		return cmp.Or(
			strings.Compare(a.key.Kind, b.key.Kind),
			strings.Compare(a.key.Namespace, b.key.Namespace),
			strings.Compare(a.key.Name, b.key.Name),
		)
	})

	return entries
}

// printResourceEntries prints already-processed resources, each with a box
// header (same format as diff output).
func printResourceEntries(entries []resourceEntry) {
	for _, e := range entries {
		header := e.key.String()
		border := strings.Repeat("-", len(header)+2)
		fmt.Printf("%s\n %s\n%s\n%s\n\n", border, header, border, e.content)
	}
}

// reorderYAMLFields reorders top-level YAML fields to Kubernetes convention:
// apiVersion, kind, metadata first, then other fields in original order.
// Also strips SOPS metadata from Secret resources.
func reorderYAMLFields(data []byte) []byte {
	if len(data) == 0 {
		return data
	}

	docs := flux.SplitYAMLText(data)
	var result []string

	for _, doc := range docs {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		processed := processYAMLDoc([]byte(doc))
		if len(bytes.TrimSpace(processed)) > 0 {
			result = append(result, strings.TrimSpace(string(processed)))
		}
	}

	return []byte(strings.Join(result, "\n---\n"))
}

// processYAMLDoc parses a single YAML document, strips SOPS metadata,
// reorders top-level keys (apiVersion, kind, metadata first), and encodes
// the result. Uses yaml.Node for correct parsing (unlike the previous
// text-based approach which was fragile with block scalars, comments,
// and quoting styles).
//
// A document that is already canonical — no sops key, apiVersion/kind/
// metadata leading in order — is returned untouched: re-encoding it would
// cost a full marshal for zero effect and only risk formatting drift.
func processYAMLDoc(doc []byte) []byte {
	var node yaml.Node
	if err := yaml.Unmarshal(doc, &node); err != nil {
		return doc // can't parse, return as-is
	}

	mapping := mappingNode(&node)
	if mapping == nil {
		return doc
	}

	if !hasMapKey(mapping, "sops") && canonicalKeyOrder(mapping) {
		return doc
	}

	removeMapKey(mapping, "sops")
	reorderMapKeys(mapping, []string{"apiVersion", "kind", "metadata"})

	return encodeNode(&node)
}

// encodeNode encodes a document tree at the pipeline's canonical 2-space
// indent.
func encodeNode(node *yaml.Node) []byte {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	_ = enc.Encode(node)
	enc.Close()
	return buf.Bytes()
}

// hasMapKey reports whether the mapping carries the key.
func hasMapKey(mapping *yaml.Node, key string) bool {
	for i := 0; i < len(mapping.Content)-1; i += 2 {
		if mapping.Content[i].Value == key {
			return true
		}
	}
	return false
}

// canonicalKeyOrder reports whether the mapping's leading keys are exactly
// apiVersion, kind, metadata in that order.
func canonicalKeyOrder(mapping *yaml.Node) bool {
	for i, want := range []string{"apiVersion", "kind", "metadata"} {
		k := 2 * i
		if k >= len(mapping.Content) || mapping.Content[k].Value != want {
			return false
		}
	}
	return true
}

// mappingNode returns the MappingNode inside a DocumentNode, or nil.
func mappingNode(doc *yaml.Node) *yaml.Node {
	if doc == nil || doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil
	}
	if doc.Content[0].Kind != yaml.MappingNode {
		return nil
	}
	return doc.Content[0]
}

// removeMapKey removes a key (and its value) from a MappingNode.
func removeMapKey(mapping *yaml.Node, key string) {
	for i := 0; i < len(mapping.Content)-1; i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content = append(mapping.Content[:i], mapping.Content[i+2:]...)
			return
		}
	}
}

// reorderMapKeys reorders key-value pairs in a MappingNode so that keys in
// the priority list come first (in list order), followed by remaining keys
// in their original order.
func reorderMapKeys(mapping *yaml.Node, priority []string) {
	type pair struct{ key, val *yaml.Node }

	var pairs []pair
	for i := 0; i < len(mapping.Content)-1; i += 2 {
		pairs = append(pairs, pair{key: mapping.Content[i], val: mapping.Content[i+1]})
	}

	seen := make(map[string]bool)
	var ordered []pair

	for _, pk := range priority {
		for _, p := range pairs {
			if p.key.Value == pk && !seen[pk] {
				ordered = append(ordered, p)
				seen[pk] = true
				break
			}
		}
	}
	for _, p := range pairs {
		if !seen[p.key.Value] {
			ordered = append(ordered, p)
			seen[p.key.Value] = true
		}
	}

	mapping.Content = nil
	for _, p := range ordered {
		mapping.Content = append(mapping.Content, p.key, p.val)
	}
}
