package flux

import (
	"fmt"
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

// ApplySubstitution replaces ${VAR} and $(VAR) patterns in YAML content with resolved values.
func ApplySubstitution(data []byte, vars map[string]string) []byte {
	if len(vars) == 0 {
		return data
	}

	result := string(data)
	for key, value := range vars {
		result = strings.ReplaceAll(result, "${"+key+"}", value)
		result = strings.ReplaceAll(result, "$("+key+")", value)
	}

	return []byte(result)
}

// ParseSubstituteFrom parses the substituteFrom field (exported for testing).
func ParseSubstituteFrom(raw interface{}) []substituteFromEntry {
	return parseSubstituteFrom(raw)
}

// parseSubstituteFrom parses the substituteFrom field which can be a list of objects.
func parseSubstituteFrom(raw interface{}) []substituteFromEntry {
	if raw == nil {
		return nil
	}

	var entries []substituteFromEntry

	switch v := raw.(type) {
	case []interface{}:
		for _, item := range v {
			if m, ok := item.(map[string]interface{}); ok {
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

// SubstituteNeeded returns true if the Kustomization has postBuild substitution configured.
func SubstituteNeeded(ks Kustomization) bool {
	return ks.Spec.PostBuild != nil && !ks.Spec.PostBuild.DisableSubstitute &&
		(len(ks.Spec.PostBuild.Substitute) > 0 || ks.Spec.PostBuild.SubstituteFrom != nil)
}
