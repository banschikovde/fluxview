package flux

import (
	"fmt"
	"regexp"
	"strings"
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

// TopologicalSort sorts Kustomizations by their dependsOn dependencies.
// Returns an ordered list where dependencies come before dependents.
// Returns an error if a circular dependency is detected.
func TopologicalSort(kustomizations []Kustomization) ([]Kustomization, error) {
	// Build a name -> index map.
	nameToIdx := make(map[string]int)
	for i, ks := range kustomizations {
		key := ks.Metadata.Namespace + "/" + ks.Metadata.Name
		nameToIdx[key] = i
	}

	// Build adjacency list (dependency graph).
	// edge from A -> B means A depends on B (B must come before A).
	graph := make(map[int][]int)
	inDegree := make(map[int]int)

	for i := range kustomizations {
		inDegree[i] = 0
	}

	for i, ks := range kustomizations {
		for _, dep := range ks.Spec.DependsOn {
			depNS := dep.Namespace
			if depNS == "" {
				depNS = ks.Metadata.Namespace
			}
			depKey := depNS + "/" + dep.Name
			if depIdx, ok := nameToIdx[depKey]; ok {
				graph[depIdx] = append(graph[depIdx], i)
				inDegree[i]++
			}
			// If dependency not found in our list, ignore it
			// (might be external or already processed)
		}
	}

	// Kahn's algorithm for topological sort.
	var queue []int
	for i := range kustomizations {
		if inDegree[i] == 0 {
			queue = append(queue, i)
		}
	}

	var sorted []Kustomization
	for len(queue) > 0 {
		idx := queue[0]
		queue = queue[1:]
		sorted = append(sorted, kustomizations[idx])

		for _, next := range graph[idx] {
			inDegree[next]--
			if inDegree[next] == 0 {
				queue = append(queue, next)
			}
		}
	}

	if len(sorted) != len(kustomizations) {
		return nil, fmt.Errorf("circular dependency detected in Kustomization resources")
	}

	return sorted, nil
}

// TopologicalSortHelmReleases sorts HelmReleases by their dependsOn dependencies.
// Returns an ordered list where dependencies come before dependents.
// Returns an error if a circular dependency is detected.
func TopologicalSortHelmReleases(helmReleases []HelmRelease) ([]HelmRelease, error) {
	// Build a name -> index map.
	nameToIdx := make(map[string]int)
	for i, hr := range helmReleases {
		key := hr.Metadata.Namespace + "/" + hr.Metadata.Name
		nameToIdx[key] = i
	}

	// Build adjacency list (dependency graph).
	// edge from A -> B means A depends on B (B must come before A).
	graph := make(map[int][]int)
	inDegree := make(map[int]int)

	for i := range helmReleases {
		inDegree[i] = 0
	}

	for i, hr := range helmReleases {
		for _, dep := range hr.Spec.DependsOn {
			depNS := dep.Namespace
			if depNS == "" {
				depNS = hr.Metadata.Namespace
			}
			depKey := depNS + "/" + dep.Name
			if depIdx, ok := nameToIdx[depKey]; ok {
				graph[depIdx] = append(graph[depIdx], i)
				inDegree[i]++
			}
			// If dependency not found in our list, ignore it
			// (might be external or already processed)
		}
	}

	// Kahn's algorithm for topological sort.
	var queue []int
	for i := range helmReleases {
		if inDegree[i] == 0 {
			queue = append(queue, i)
		}
	}

	var sorted []HelmRelease
	for len(queue) > 0 {
		idx := queue[0]
		queue = queue[1:]
		sorted = append(sorted, helmReleases[idx])

		for _, next := range graph[idx] {
			inDegree[next]--
			if inDegree[next] == 0 {
				queue = append(queue, next)
			}
		}
	}

	if len(sorted) != len(helmReleases) {
		return nil, fmt.Errorf("circular dependency detected in HelmRelease resources")
	}

	return sorted, nil
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
// (matching Flux behavior where the order matters). ConfigMap and Secret values are merged
// by key, not by type precedence.
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

		switch strings.ToLower(entry.Kind) {
		case "configmap":
			for _, cm := range configMaps {
				if cm.Metadata.Name == entry.Name && cm.Metadata.Namespace == entryNS {
					for k, v := range cm.Data {
						result[k] = v
					}
					break
				}
			}
		case "secret":
			for _, secret := range secrets {
				if secret.Metadata.Name == entry.Name && secret.Metadata.Namespace == entryNS {
					for k := range secret.Data {
						decodedValue := secret.GetSecretValue(k)
						if decodedValue != "" {
							result[k] = decodedValue
						}
					}
					// Also handle stringData (keys not in data)
					if secret.StringData != nil {
						for k, v := range secret.StringData {
							if _, exists := secret.Data[k]; !exists {
								result[k] = v
							}
						}
					}
					break
				}
			}
		}
	}

	return result
}

// SubstituteNeeded returns true if the Kustomization has postBuild substitution configured.
func SubstituteNeeded(ks Kustomization) bool {
	return ks.Spec.PostBuild != nil && !ks.Spec.PostBuild.DisableSubstitute &&
		(len(ks.Spec.PostBuild.Substitute) > 0 || ks.Spec.PostBuild.SubstituteFrom != nil)
}
