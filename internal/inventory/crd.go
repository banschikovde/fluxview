package inventory

import (
	"fmt"
	"strings"

	"github.com/Masterminds/semver/v3"
)

// CRDComponents groups CustomResourceDefinition documents by API group into
// one component per group — CRDs installed outside the operator's chart are
// separately versioned artifacts (renovate-pinned release URLs, upstream
// git tags) that can drift from the operator.
//
// Lean by design: only groups with a KNOWN version produce components —
// from the app.kubernetes.io/version label or, when the CRDs ship from an
// external GitRepository with a pinned tag, from that tag (gitRefs maps API
// group → tag). Groups with neither are invisible: a CRD is a schema, not
// software, and rows without a version would be noise. Diverging versions
// within a group resolve to the highest and add a warning.
func CRDComponents(raws []map[string]interface{}, gitRefs map[string]string) []Component {
	type entry struct {
		version string
		source  string
	}
	groups := map[string][]entry{}
	order := []string{}

	for _, raw := range raws {
		if kind, _ := raw["kind"].(string); kind != "CustomResourceDefinition" {
			continue
		}
		spec, _ := raw["spec"].(map[string]interface{})
		if spec == nil {
			continue
		}
		group, _ := spec["group"].(string)
		if group == "" {
			continue
		}
		if _, seen := groups[group]; !seen {
			order = append(order, group)
		}

		version, source := "", ""
		if metadata, ok := raw["metadata"].(map[string]interface{}); ok {
			if labels, ok := metadata["labels"].(map[string]interface{}); ok {
				if v, ok := labels["app.kubernetes.io/version"].(string); ok && v != "" {
					version, source = v, VersionLabel
				}
			}
		}
		if version == "" {
			if v, ok := gitRefs[group]; ok && v != "" {
				version, source = v, VersionGitRef
			}
		}
		if version == "" {
			continue // unknown version — invisible
		}
		groups[group] = append(groups[group], entry{version: version, source: source})
	}

	components := make([]Component, 0, len(groups))
	for _, group := range order {
		entries := groups[group]
		if len(entries) == 0 {
			continue
		}
		c := Component{
			Source: SourceCRD,
			Kind:   "CustomResourceDefinition",
			Name:   fmt.Sprintf("%s (%d CRD)", group, len(entries)),
		}
		versions := map[string]bool{}
		for _, e := range entries {
			versions[e.version] = true
		}
		if len(versions) == 1 {
			c.Version = entries[0].version
			c.VersionSource = entries[0].source
		} else {
			c.Version = highestVersion(versions)
			c.VersionSource = entries[0].source
			c.Warnings = append(c.Warnings, fmt.Sprintf("CRDs in group %s have different versions, showing the highest", group))
		}
		components = append(components, c)
	}
	return components
}

// highestVersion picks the highest version from the set by a total order:
// a semver-parsable version outranks an unparsable one, parsable versions
// compare by semver with a string tie-break ("1.0" vs "1.0.0"), unparsable
// ones by plain string. A total order makes the choice independent of map
// iteration — the determinism the golden tests rely on.
func highestVersion(versions map[string]bool) string {
	best := ""
	var bestSV *semver.Version
	better := func(a string, asv *semver.Version, b string, bsv *semver.Version) bool {
		switch {
		case asv != nil && bsv == nil:
			return true // parsable outranks unparsable — both directions
		case asv == nil && bsv != nil:
			return false
		case asv != nil && bsv != nil:
			if !asv.Equal(bsv) {
				return asv.GreaterThan(bsv)
			}
			return a > b // semver-equal: deterministic string tie-break
		default:
			return a > b
		}
	}
	for v := range versions {
		sv, err := semver.NewVersion(strings.TrimPrefix(v, "v"))
		if err != nil {
			sv = nil
		}
		if best == "" || better(v, sv, best, bestSV) {
			best, bestSV = v, sv
		}
	}
	return best
}
