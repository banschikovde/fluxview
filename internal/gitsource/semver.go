package gitsource

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Masterminds/semver/v3"
	"github.com/go-git/go-git/v5/plumbing"
)

// pickSemverTag selects the highest git tag satisfying the Flux semver
// constraint from an ls-remote reference list. A leading "v" (also "V") on
// tags is tolerated, matching source-controller; peeled tag entries
// (refs/tags/<name>^{}) are ignored — the tag itself carries the name being
// selected. Prerelease handling follows Masterminds/semver constraint
// semantics (a constraint without a prerelease does not match prerelease
// tags unless it says so, e.g. ">=1.0.0-0").
func pickSemverTag(refs []*plumbing.Reference, constraint string) (string, error) {
	c, err := semver.NewConstraint(constraint)
	if err != nil {
		return "", fmt.Errorf("invalid semver constraint: %w", err)
	}

	var bestTag string
	var bestVersion *semver.Version
	var allTags []string
	for _, ref := range refs {
		name := ref.Name().String()
		tag, ok := strings.CutPrefix(name, "refs/tags/")
		if !ok || strings.HasSuffix(tag, "^{}") {
			continue
		}
		allTags = append(allTags, tag)
		v, err := semver.StrictNewVersion(strings.TrimPrefix(strings.TrimPrefix(tag, "v"), "V"))
		if err != nil {
			continue
		}
		if !c.Check(v) {
			continue
		}
		if bestVersion == nil || v.GreaterThan(bestVersion) {
			bestTag, bestVersion = tag, v
		}
	}
	if bestVersion == nil {
		sort.Strings(allTags)
		return "", fmt.Errorf("no tag matches constraint %q (available: %s)",
			constraint, strings.Join(allTags, ", "))
	}
	return bestTag, nil
}
