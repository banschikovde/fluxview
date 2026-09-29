package inventory

import (
	"fmt"
)

// identity keys a component across revisions: source, namespace, name AND
// kind. Kind is part of the key because one name can carry several
// components — a VMCluster and the VMAuth fronting it — and without it the
// diff maps would collapse them, comparing a VLAgent against a VMAuth and
// fabricating version markers. A kind change is therefore an honest
// remove+add pair.
func identity(c Component) string {
	return c.Source + "\x00" + c.Namespace + "\x00" + c.Name + "\x00" + c.Kind
}

// DiffComponents compares two component snapshots and annotates the union
// with Change markers for --branch-orig: "+" added (only in after), "-"
// removed (only in before), "~ old → new" version updated. Unchanged
// components pass through with an empty Change. The result is sorted by the
// effective sort keys plus the change marker, so added/removed pairs of the
// same component group together.
func DiffComponents(before, after []Component, sortKeys []string) []Component {
	beforeByID := make(map[string]Component, len(before))
	for _, c := range before {
		beforeByID[identity(c)] = c
	}
	afterByID := make(map[string]Component, len(after))
	for _, c := range after {
		afterByID[identity(c)] = c
	}

	out := make([]Component, 0, len(before)+len(after))
	for _, c := range after {
		old, existed := beforeByID[identity(c)]
		switch {
		case !existed:
			c.Change = "+"
		case old.Version != c.Version:
			c.Change = fmt.Sprintf("~ %s → %s", old.Version, c.Version)
		}
		out = append(out, c)
	}
	for _, c := range before {
		if _, still := afterByID[identity(c)]; !still {
			removed := c
			removed.Change = "-"
			out = append(out, removed)
		}
	}

	Sort(out, sortKeys)
	return out
}

// HasChanges reports whether a diffed component list contains at least one
// changed component (drives the --branch-orig exit code 1).
func HasChanges(components []Component) bool {
	for _, c := range components {
		if c.Change != "" {
			return true
		}
	}
	return false
}
