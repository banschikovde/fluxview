package yamlutil

import (
	"strings"

	"gopkg.in/yaml.v3"
)

// DedupDocs removes duplicate YAML documents by API group/kind/namespace/name,
// keeping the LAST occurrence (matching kustomize ResMap semantics). Documents
// that fail to parse or are not k8s resources (no kind or no metadata.name)
// are kept as-is. Returns nil when no documents remain.
//
// Single shared implementation for the KS pipeline output
// (buildKSContent) and the in-memory kustomize builds
// (runInMemoryBuild — kustomize's resource accumulator rejects duplicate
// IDs even across separate files).
func DedupDocs(data []byte) []byte {
	docs := SplitYAMLText(data)
	type resourceKey struct {
		group, kind, namespace, name string
	}
	seen := make(map[resourceKey]int) // key → index in result
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
		if err := yaml.Unmarshal([]byte(doc), &meta); err != nil || meta.Kind == "" || meta.Metadata.Name == "" {
			result = append(result, doc) // unparseable or not a resource — keep
			continue
		}

		// Extract group from apiVersion (e.g. "apps/v1" → "apps", "v1" → "").
		group := ""
		if idx := strings.Index(meta.APIVersion, "/"); idx > 0 {
			group = meta.APIVersion[:idx]
		}

		key := resourceKey{group, meta.Kind, meta.Metadata.Namespace, meta.Metadata.Name}
		if idx, ok := seen[key]; ok {
			result[idx] = doc // replace existing occurrence (last wins)
		} else {
			seen[key] = len(result)
			result = append(result, doc)
		}
	}

	if len(result) == 0 {
		return nil
	}
	return []byte(strings.Join(result, "\n---\n"))
}
