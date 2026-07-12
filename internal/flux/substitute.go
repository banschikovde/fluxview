package flux

import (
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// substituteFromEntry represents a single entry in the postBuild.substituteFrom list.
type substituteFromEntry struct {
	Kind      string `yaml:"kind"`
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace,omitempty"`
}

// ResolveSubstituteVars resolves all substitution variables for a Kustomization.
// It collects variables from:
// 1. spec.postBuild.substitute (inline vars)
// 2. spec.postBuild.substituteFrom (ConfigMap references, resolved from parsed ConfigMaps)
func ResolveSubstituteVars(ks Kustomization, configMaps []ConfigMap) map[string]string {
	vars := make(map[string]string)

	if ks.Spec.PostBuild == nil || ks.Spec.PostBuild.DisableSubstitute {
		return vars
	}

	// Resolve substituteFrom references.
	entries := parseSubstituteFrom(ks.Spec.PostBuild.SubstituteFrom)
	for _, entry := range entries {
		ns := entry.Namespace
		if ns == "" {
			ns = ks.Metadata.Namespace
		}

		switch strings.ToLower(entry.Kind) {
		case "configmap":
			for _, cm := range configMaps {
				if cm.Metadata.Name == entry.Name && cm.Metadata.Namespace == ns {
					for k, v := range cm.Data {
						vars[k] = v
					}
				}
			}
		case "secret":
			// We cannot read actual secret values; use placeholders.
			// This is consistent with Flux behavior where secrets are available
			// at runtime but we don't have access to them locally.
		}
	}

	// Inline substitute values override substituteFrom.
	for k, v := range ks.Spec.PostBuild.Substitute {
		vars[k] = v
	}

	return vars
}

// varPattern matches ${VAR}, ${VAR:=default}, ${VAR:-default}.
var varPattern = regexp.MustCompile(`\$\{([^}]+)\}`)

// ApplySubstitution replaces ${VAR}, ${VAR:=default}, ${VAR:-default},
// and $(VAR) patterns in YAML content with resolved values.
// Unresolved variables without a default are replaced with empty string,
// matching Flux postBuild substitution behavior.
func ApplySubstitution(data []byte, vars map[string]string) []byte {
	if len(vars) == 0 {
		return data
	}

	// Handle ${...} patterns (including defaults).
	result := varPattern.ReplaceAllStringFunc(string(data), func(match string) string {
		inner := match[2 : len(match)-1] // strip ${ and }

		// Check for := (assign default) or :- (use default if unset/empty).
		// In bash (and Flux), both operators trigger on unset AND empty values.
		for _, sep := range []string{":=", ":-"} {
			if idx := strings.Index(inner, sep); idx >= 0 {
				key := inner[:idx]
				defaultVal := inner[idx+2:]
				if val, ok := vars[key]; ok && val != "" {
					return val
				}
				return defaultVal
			}
		}

		// Simple ${VAR} — Flux substitutes empty string for unresolved vars.
		if val, ok := vars[inner]; ok {
			return val
		}
		return ""
	})

	// Handle $(VAR) syntax.
	for key, value := range vars {
		result = strings.ReplaceAll(result, "$("+key+")", value)
	}

	return []byte(result)
}

// parseSubstituteFrom parses the substituteFrom field which can be a list of objects.
func parseSubstituteFrom(raw any) []substituteFromEntry {
	if raw == nil {
		return nil
	}

	var entries []substituteFromEntry

	switch v := raw.(type) {
	case []any:
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				entry := substituteFromEntry{}
				if kind, ok := m["kind"].(string); ok {
					entry.Kind = kind
				}
				if name, ok := m["name"].(string); ok {
					entry.Name = name
				}
				if ns, ok := m["namespace"].(string); ok {
					entry.Namespace = ns
				}
				entries = append(entries, entry)
			}
		}
	}

	return entries
}

// dependencyNode is the interface for topological sort.
// Each type resolves its own dependency keys (including namespace fallback logic).
type dependencyNode interface {
	ident() string     // "namespace/name"
	depKeys() []string // resolved dependency keys
}

func topologicalSortGeneric[T dependencyNode](items []T, typeName string) ([]T, error) {
	idxMap := make(map[string]int)
	for i, item := range items {
		idxMap[item.ident()] = i
	}

	graph := make(map[int][]int)
	inDegree := make(map[int]int)
	for i := range items {
		inDegree[i] = 0
	}

	for i, item := range items {
		for _, depKey := range item.depKeys() {
			if depIdx, ok := idxMap[depKey]; ok {
				graph[depIdx] = append(graph[depIdx], i)
				inDegree[i]++
			}
		}
	}

	var queue []int
	for i := range items {
		if inDegree[i] == 0 {
			queue = append(queue, i)
		}
	}

	var sorted []T
	for len(queue) > 0 {
		idx := queue[0]
		queue = queue[1:]
		sorted = append(sorted, items[idx])

		for _, next := range graph[idx] {
			inDegree[next]--
			if inDegree[next] == 0 {
				queue = append(queue, next)
			}
		}
	}

	if len(sorted) != len(items) {
		return nil, fmt.Errorf("circular dependency detected in %s resources", typeName)
	}

	return sorted, nil
}

// TopologicalSort sorts Kustomizations by their dependsOn dependencies.
func TopologicalSort(items []Kustomization) ([]Kustomization, error) {
	return topologicalSortGeneric(items, "Kustomization")
}

// TopologicalSortHelmReleases sorts HelmReleases by their dependsOn dependencies.
func TopologicalSortHelmReleases(items []HelmRelease) ([]HelmRelease, error) {
	return topologicalSortGeneric(items, "HelmRelease")
}

// parseValuesFrom parses the valuesFrom field which can be a list of objects.
func parseValuesFrom(raw any) []ValuesFromEntry {
	if raw == nil {
		return nil
	}

	var entries []ValuesFromEntry

	switch v := raw.(type) {
	case []any:
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				entry := ValuesFromEntry{}
				if kind, ok := m["kind"].(string); ok {
					entry.Kind = kind
				}
				if name, ok := m["name"].(string); ok {
					entry.Name = name
				}
				if ns, ok := m["namespace"].(string); ok {
					entry.Namespace = ns
				}
				if vk, ok := m["valuesKey"].(string); ok {
					entry.ValuesKey = vk
				}
				if optional, ok := m["optional"].(bool); ok {
					entry.Optional = optional
				}
				entries = append(entries, entry)
			}
		}
	}

	return entries
}

// ResolveValuesFrom resolves values from ConfigMaps and Secrets referenced in valuesFrom.
// Returns a merged map of values where later entries in the valuesFrom list override earlier ones
// (matching Flux behavior where the order matters).
//
// ConfigMap values are parsed as YAML and merged. Secret values are parsed
// structurally but every leaf is replaced with SecretRedactedValue — this
// ensures chart templates render correctly while real secret values never
// appear in the output.
func ResolveValuesFrom(hr HelmRelease, configMaps []ConfigMap, secrets []Secret) map[string]any {
	entries := parseValuesFrom(hr.Spec.ValuesFrom)
	if len(entries) == 0 {
		return nil
	}

	result := make(map[string]any)

	for _, entry := range entries {
		entryNS := entry.Namespace
		if entryNS == "" {
			entryNS = hr.Metadata.Namespace
		}
		vk := entry.ValuesKey
		if vk == "" {
			vk = "values.yaml"
		}

		switch strings.ToLower(entry.Kind) {
		case "configmap":
			found := false
			for _, cm := range configMaps {
				if cm.Metadata.Name == entry.Name && cm.Metadata.Namespace == entryNS {
					mergeConfigMapValues(result, cm.Data, vk)
					found = true
					break
				}
			}
			if !found {
				for _, cm := range configMaps {
					if cm.Metadata.Name == entry.Name && cm.Metadata.Namespace == "" {
						mergeConfigMapValues(result, cm.Data, vk)
						break
					}
				}
			}
		case "secret":
			found := false
			for _, secret := range secrets {
				if secret.Metadata.Name == entry.Name && secret.Metadata.Namespace == entryNS {
					mergeSecretPlaceholder(result, secret, vk)
					found = true
					break
				}
			}
			if !found {
				for _, secret := range secrets {
					if secret.Metadata.Name == entry.Name && secret.Metadata.Namespace == "" {
						mergeSecretPlaceholder(result, secret, vk)
						break
					}
				}
			}
		}
	}

	return result
}

// mergeConfigMapValues selects the value at valuesKey from ConfigMap data,
// parses it as YAML, and merges the top-level keys into result.
// This matches Flux HelmController behavior: the ConfigMap data key
// (default "values.yaml") contains a YAML document whose keys become
// Helm chart values.
func mergeConfigMapValues(result map[string]any, data map[string]string, valuesKey string) {
	raw, ok := data[valuesKey]
	if !ok {
		return
	}
	mergeYAMLString(result, raw)
}

// mergeYAMLString parses a YAML string and merges its top-level keys into dst.
// If the string is a scalar (not a map), it is silently skipped.
func mergeYAMLString(dst map[string]any, raw string) {
	var parsed map[string]interface{}
	if err := yaml.Unmarshal([]byte(raw), &parsed); err != nil {
		return
	}
	for k, v := range parsed {
		dst[k] = v
	}
}

// mergeSecretPlaceholder injects placeholder values for secret-based valuesFrom.
// It reads the YAML structure from the secret's data key (same as ConfigMap),
// but replaces every leaf value with SecretRedactedValue. This ensures:
//   - Chart templates that reference these values render correctly
//   - Diff detects changes to secret structure
//   - Real secret values NEVER appear in the rendered output
func mergeSecretPlaceholder(result map[string]any, secret Secret, valuesKey string) {
	// Use GetSecretValue which handles both stringData (plaintext) and
	// data (base64-encoded), matching real-world Kubernetes Secret manifests.
	raw := secret.GetSecretValue(valuesKey)
	if raw == "" {
		return
	}

	var parsed map[string]interface{}
	if err := yaml.Unmarshal([]byte(raw), &parsed); err != nil {
		return
	}

	for k, v := range parsed {
		result[k] = redactRecursive(v)
	}
}

// redactRecursive replaces all scalar leaf values with a YAML-safe placeholder,
// preserving the structure of nested maps and lists.
func redactRecursive(v any) any {
	switch val := v.(type) {
	case map[string]interface{}:
		result := make(map[string]interface{})
		for k, child := range val {
			result[k] = redactRecursive(child)
		}
		return result
	case []interface{}:
		result := make([]interface{}, len(val))
		for i, child := range val {
			result[i] = redactRecursive(child)
		}
		return result
	default:
		return SecretHelmPlaceholder
	}
}

// SubstituteNeeded returns true if the Kustomization has postBuild substitution configured.
func SubstituteNeeded(ks Kustomization) bool {
	return ks.Spec.PostBuild != nil && !ks.Spec.PostBuild.DisableSubstitute &&
		(len(ks.Spec.PostBuild.Substitute) > 0 || ks.Spec.PostBuild.SubstituteFrom != nil)
}
