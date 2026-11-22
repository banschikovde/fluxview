package ocisource

import (
	"fmt"
	"strings"

	"github.com/Masterminds/semver/v3"
)

// pickSemverTag returns the highest tag matching the Flux semver
// constraint, mirroring source-controller's getTagBySemver: tags are
// parsed as semver with a tolerated v/V prefix, and a "_" in a tag reads
// as the "+" it had to be mangled into for registries that reject "+"
// (the same tolerance the Helm chart path applies). No matching tag is an
// error naming the constraint and the available tags.
func pickSemverTag(tags []string, constraint string) (string, error) {
	c, err := semver.NewConstraint(constraint)
	if err != nil {
		return "", fmt.Errorf("invalid semver constraint %q: %w", constraint, err)
	}
	var best *semver.Version
	bestTag := ""
	for _, tag := range tags {
		v, err := semver.NewVersion(tag)
		if err != nil {
			if plus, perr := semver.NewVersion(strings.ReplaceAll(tag, "_", "+")); perr == nil {
				v, err = plus, nil
			}
		}
		if err != nil {
			continue
		}
		if !c.Check(v) {
			continue
		}
		if best == nil || v.GreaterThan(best) {
			best = v
			bestTag = tag
		}
	}
	if best == nil {
		return "", fmt.Errorf("no tag matches constraint %q (available: %s)", constraint, renderTags(tags))
	}
	return bestTag, nil
}

// maxTagsInError caps the tag list in the no-match error; longer lists
// collapse to the first 20 and a count.
const maxTagsInError = 20

// renderTags renders up to maxTagsInError tags plus an "and N more" tail.
func renderTags(tags []string) string {
	if len(tags) <= maxTagsInError {
		return strings.Join(tags, ", ")
	}
	return strings.Join(tags[:maxTagsInError], ", ") + fmt.Sprintf(", … and %d more", len(tags)-maxTagsInError)
}
