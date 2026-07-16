// Package yamlutil provides shared YAML text helpers used across packages.
//
// It exists as a neutral, dependency-free location so that otherwise-cyclic
// packages (e.g. internal/flux, which is imported by internal/kustomize, and
// internal/kustomize) can share the same multi-document splitting logic
// without duplicating it.
package yamlutil

import "strings"

// SplitYAMLText splits multi-doc YAML into individual documents.
//
// A document separator is a line that is exactly "---" at column 0
// (no indentation). This correctly avoids splitting on "---" that appears:
//   - Inside block scalars (| or >) — content is always indented
//   - Inside PEM headers like "-----BEGIN CERTIFICATE-----"
//   - As part of a longer string value
//
// Unparseable documents are preserved (conservative behavior) so that
// downstream functions like RedactSecrets can process all documents.
func SplitYAMLText(data []byte) []string {
	normalized := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(normalized, "\n")

	var docs []string
	var currentLines []string

	flush := func() {
		if len(currentLines) == 0 {
			return
		}
		doc := strings.TrimSpace(strings.Join(currentLines, "\n"))
		doc = strings.TrimPrefix(doc, "---")
		doc = strings.TrimSpace(doc)
		if doc != "" {
			docs = append(docs, doc)
		}
		currentLines = nil
	}

	for _, line := range lines {
		// A document separator is "---" at column 0, optionally with
		// trailing whitespace. Indented "---" (inside block scalars) or
		// longer strings like "-----BEGIN" are NOT separators.
		if strings.TrimRight(line, " \t") == "---" {
			flush()
			continue
		}
		currentLines = append(currentLines, line)
	}
	flush()

	return docs
}
