package yamlutil

// MergeMaps deep-merges src into dst in place: keys whose values are maps
// on both sides merge recursively, everything else (scalars, lists, nil,
// mismatched types) is overwritten by src. This is the layering semantics
// helm-controller applies between HelmRelease values layers
// (valuesFiles < valuesFrom < values) before the Helm SDK coalesces the
// result with the chart's values.yaml — a top-level overwrite would
// silently drop sibling keys of nested maps from earlier layers.
func MergeMaps(dst, src map[string]any) {
	for k, v := range src {
		if sm, ok := v.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				MergeMaps(dm, sm)
				continue
			}
		}
		dst[k] = v
	}
}
