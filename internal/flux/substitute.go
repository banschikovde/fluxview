package flux

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/banschikovde/fluxview/internal/yamlutil"
)

// substituteFromEntry represents a single entry in the postBuild.substituteFrom list.
type substituteFromEntry struct {
	Kind      string `yaml:"kind"`
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace,omitempty"`
	// Optional: when true, a missing referenced resource is silently ignored
	// (no warning), matching Flux behavior.
	Optional bool `yaml:"optional,omitempty"`
}

// ResolveSubstituteVars resolves all substitution variables for a Kustomization.
// It collects variables from:
// 1. spec.postBuild.substitute (inline vars)
// 2. spec.postBuild.substituteFrom:
//   - ConfigMap: real values from parsed ConfigMap.Data
//   - Secret: SecretHelmPlaceholder injected for every key in Data/StringData
//     (real secret values are never read locally; only key names are used).
//
// The Secret placeholder mirrors valuesFrom handling (mergeSecretPlaceholder /
// redactRecursive): ${VAR} resolves to a non-empty YAML-safe string instead of
// being dropped to "" (which YAML parses as null and breaks chart templates
// that expect a string).
func ResolveSubstituteVars(ks Kustomization, configMaps []ConfigMap, secrets []Secret) map[string]string {
	vars := make(map[string]string)

	if ks.Spec.PostBuild == nil || ks.Spec.PostBuild.DisableSubstitute {
		return vars
	}

	// Resolve substituteFrom references.
	entries := parseSubstituteFrom(ks.Spec.PostBuild.SubstituteFrom)
	for _, entry := range entries {
		ns := entry.Namespace
		isExplicitNS := ns != ""
		if ns == "" {
			ns = ks.Metadata.Namespace
		}
		// Same restricted fallback logic as ResolveValuesFrom:
		// empty-namespace resources match as fallback only when namespace
		// was not explicitly set in substituteFrom.
		allowFallback := !isExplicitNS

		switch strings.ToLower(entry.Kind) {
		case "configmap":
			cm, ok := findCandidate(configMaps, entry.Name, ns, allowFallback, func(c ConfigMap) ObjectMeta { return c.Metadata })
			if !ok {
				if !entry.Optional {
					fmt.Fprintf(os.Stderr, "Warning: substituteFrom ConfigMap %s/%s not found (referenced by Kustomization %s/%s)\n",
						ns, entry.Name, ks.Metadata.Namespace, ks.Metadata.Name)
				}
				continue
			}
			for k, v := range cm.Data {
				vars[k] = v
			}
		case "secret":
			// Real secret values aren't available locally, but the key names
			// are (from the parsed Secret resource). Inject SecretHelmPlaceholder
			// for each key so unresolved ${VAR} references sourced from this
			// Secret resolve to a non-empty YAML-safe string instead of being
			// silently dropped to null.
			secret, ok := findCandidate(secrets, entry.Name, ns, allowFallback, func(s Secret) ObjectMeta { return s.Metadata })
			if !ok {
				if !entry.Optional {
					fmt.Fprintf(os.Stderr, "Warning: substituteFrom Secret %s/%s not found (referenced by Kustomization %s/%s)\n",
						ns, entry.Name, ks.Metadata.Namespace, ks.Metadata.Name)
				}
				continue
			}
			for k := range secret.Data {
				vars[k] = SecretHelmPlaceholder
			}
			for k := range secret.StringData {
				vars[k] = SecretHelmPlaceholder
			}
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

// parenVarPattern matches $(VAR).
var parenVarPattern = regexp.MustCompile(`\$\(([^)]+)\)`)

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

	// Handle $(VAR) syntax in a single pass. Unresolved $(VAR) are left as-is:
	// unlike ${VAR} (which Flux replaces with empty string), the previous
	// strings.ReplaceAll loop only substituted variables that were actually
	// present in vars, so this preserves that behavior.
	result = parenVarPattern.ReplaceAllStringFunc(result, func(match string) string {
		key := match[2 : len(match)-1] // strip $( and )
		if val, ok := vars[key]; ok {
			return val
		}
		return match
	})

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
				if optional, ok := m["optional"].(bool); ok {
					entry.Optional = optional
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
// structurally but every leaf is replaced with SecretHelmPlaceholder — this
// ensures chart templates render correctly while real secret values never
// appear in the output.
//
// Namespace matching is strict (exact match only, no empty-namespace fallback).
// This matches real Flux behavior: the resource must be in the same namespace
// as the HelmRelease (or the namespace specified in valuesFrom). Resources
// from build output always have the correct kustomize-transformed namespace.
func ResolveValuesFrom(hr HelmRelease, configMaps []ConfigMap, secrets []Secret) map[string]any {
	entries := parseValuesFrom(hr.Spec.ValuesFrom)
	if len(entries) == 0 {
		return nil
	}

	result := make(map[string]any)

	for _, entry := range entries {
		entryNS := entry.Namespace
		isExplicitNS := entryNS != ""
		if entryNS == "" {
			entryNS = hr.Metadata.Namespace
		}
		vk := entry.ValuesKey
		if vk == "" {
			vk = "values.yaml"
		}
		// allowFallback: empty-namespace resources can match as fallback ONLY
		// when entryNS was not explicitly set in valuesFrom (i.e. it defaults
		// to the HR's namespace). This covers legitimate loose-file resources
		// without metadata.namespace (read as-is, no kustomize transform),
		// while preventing stale cross-namespace matches when valuesFrom
		// explicitly requests a specific namespace.
		allowFallback := !isExplicitNS

		switch strings.ToLower(entry.Kind) {
		case "configmap":
			cm, ok := findCandidate(configMaps, entry.Name, entryNS, allowFallback, func(c ConfigMap) ObjectMeta { return c.Metadata })
			if !ok {
				if !entry.Optional {
					fmt.Fprintf(os.Stderr, "Warning: valuesFrom ConfigMap %s/%s not found (referenced by HelmRelease %s/%s)\n",
						entryNS, entry.Name, hr.Metadata.Namespace, hr.Metadata.Name)
				}
				continue
			}
			mergeConfigMapValues(result, cm.Data, vk)

		case "secret":
			secret, ok := findCandidate(secrets, entry.Name, entryNS, allowFallback, func(s Secret) ObjectMeta { return s.Metadata })
			if !ok {
				if !entry.Optional {
					fmt.Fprintf(os.Stderr, "Warning: valuesFrom Secret %s/%s not found (referenced by HelmRelease %s/%s)\n",
						entryNS, entry.Name, hr.Metadata.Namespace, hr.Metadata.Name)
				}
				continue
			}
			mergeSecretPlaceholder(result, secret, vk)
		}
	}

	return result
}

// findCandidate finds a resource by name with exact namespace match. If
// allowFallback is true, also matches resources with empty namespace (covers
// loose-file resources without metadata.namespace). Shared by the ConfigMap
// and Secret lookups of ResolveSubstituteVars and ResolveValuesFrom.
func findCandidate[T any](items []T, name, ns string, allowFallback bool, metaOf func(T) ObjectMeta) (T, bool) {
	var fallback T
	hasFallback := false

	for _, item := range items {
		meta := metaOf(item)
		if meta.Name != name {
			continue
		}
		if meta.Namespace == ns {
			return item, true
		}
		if allowFallback && meta.Namespace == "" && !hasFallback {
			fallback = item
			hasFallback = true
		}
	}

	return fallback, hasFallback
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

// mergeYAMLString parses a YAML string and deep-merges it into dst —
// successive valuesFrom sources layer like in helm-controller, so nested
// maps from an earlier source keep their sibling keys. If the string is a
// scalar (not a map), it is silently skipped.
func mergeYAMLString(dst map[string]any, raw string) {
	var parsed map[string]any
	if err := yaml.Unmarshal([]byte(raw), &parsed); err != nil {
		return
	}
	yamlutil.MergeMaps(dst, parsed)
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

	// Redact into a copy, then deep-merge like every other valuesFrom
	// source: a later Secret entry with a nested map must not wipe sibling
	// keys contributed by an earlier entry.
	redacted := make(map[string]any, len(parsed))
	for k, v := range parsed {
		redacted[k] = redactRecursive(v)
	}
	yamlutil.MergeMaps(result, redacted)
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
