package cli

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/banschikovde/fluxview/internal/flux"
	"github.com/banschikovde/fluxview/internal/helm"
	"gopkg.in/yaml.v3"
)

// TestProcessResourcesChainParity guards the single-parse rewrite of
// processResources against the TRUE pre-rewrite chain: per document, the old
// text round-trip (strip → reorder → old unconditional Convert →
// RedactSecrets) and the new node-based pipeline must carry the same
// content. The comparison is semantic (decoded YAML values), not byte-wise:
// the rewrite intentionally keeps the authored key order of clean documents
// instead of the old alphabetical marshal.
func TestProcessResourcesChainParity(t *testing.T) {
	data := benchFixture(200)

	for _, opts := range []outputOptions{
		{},
		{skipCRDs: true},
		{stripAttrs: map[string]bool{"status": true, "creationTimestamp": true}},
	} {
		t.Run(fmt.Sprintf("opts=%+v", opts), func(t *testing.T) {
			checkProcessResourcesParity(t, data, opts, parityOldChain)
		})
	}
}

// parityOldConvert is the pre-rewrite ConvertJSONInYAMLToYAML, verbatim:
// unconditional decode + RemoveNilValues + marshal per document.
func parityOldConvert(data []byte) []byte {
	var docs []string
	for _, rawDoc := range flux.SplitYAMLText(data) {
		var doc interface{}
		if err := yaml.Unmarshal([]byte(rawDoc), &doc); err != nil || doc == nil {
			continue
		}
		marshaled, err := yaml.Marshal(helm.RemoveNilValues(doc))
		if err != nil {
			continue
		}
		docs = append(docs, strings.TrimRight(string(marshaled), "\n"))
	}
	if len(docs) == 0 {
		return nil
	}
	return []byte(strings.Join(docs, sectionSeparator))
}

// parityOldChain is the pre-rewrite processResources chain around
// parityOldConvert. reorderYAMLFields is shared with the rewrite; its fast
// path is byte-level only — the tree (sops removal, key reorder) is the
// same either way.
func parityOldChain(trimmed string, opts outputOptions) (resourceKey, interface{}, bool) {
	var meta docMeta
	if err := yaml.Unmarshal([]byte(trimmed), &meta); err != nil {
		return resourceKey{}, nil, false
	}
	if opts.namespace != "" && !meta.matchesNamespace(opts.namespace) {
		return resourceKey{}, nil, false
	}
	if opts.skipCRDs && meta.Kind == "CustomResourceDefinition" {
		return resourceKey{}, nil, false
	}
	processed := trimmed
	if len(opts.stripAttrs) > 0 {
		processed = stripAttrsFromDoc(processed, opts.stripAttrs)
	}
	if meta.Kind == "" || meta.Metadata.Name == "" {
		return resourceKey{}, nil, false
	}
	converted := parityOldConvert(reorderYAMLFields([]byte(processed)))
	if converted == nil {
		return resourceKey{}, nil, false
	}
	var v interface{}
	if err := yaml.Unmarshal(flux.RedactSecrets(converted), &v); err != nil {
		return resourceKey{}, nil, false
	}
	key := resourceKey{Kind: meta.Kind, Namespace: meta.Metadata.Namespace, Name: meta.Metadata.Name}
	return key, v, true
}

// checkProcessResourcesParity runs one option set's parity comparison:
// every document the old chain kept must survive in the new pipeline with
// semantically equal content, and no extra entries may appear.
func checkProcessResourcesParity(t *testing.T, data []byte, opts outputOptions, oldChain func(string, outputOptions) (resourceKey, interface{}, bool)) {
	t.Helper()

	newEntries := processResources(data, opts)
	newBy := map[resourceKey]interface{}{}
	for _, e := range newEntries {
		var v interface{}
		if err := yaml.Unmarshal([]byte(e.content), &v); err != nil {
			t.Fatalf("opts %+v: new content of %v is not YAML: %v", opts, e.key, err)
		}
		newBy[e.key] = v
	}

	oldCount := 0
	for _, doc := range flux.SplitYAMLText(data) {
		trimmed := string(bytes.TrimSpace([]byte(doc)))
		if trimmed == "" {
			continue
		}
		key, want, ok := oldChain(trimmed, opts)
		if !ok {
			continue
		}
		oldCount++
		got, exists := newBy[key]
		if !exists {
			t.Fatalf("opts %+v: doc %v missing in new chain", opts, key)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("opts %+v: doc %v content differs\n--- old ---\n%v\n--- new ---\n%v", opts, key, want, got)
		}
	}
	if oldCount != len(newEntries) {
		t.Fatalf("opts %+v: entry count old=%d new=%d", opts, oldCount, len(newEntries))
	}
}
