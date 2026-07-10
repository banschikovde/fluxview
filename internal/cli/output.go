package cli

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/banschikovde/fluxview/internal/flux"
	"github.com/banschikovde/fluxview/internal/helm"
)

// resourceEntry holds a single YAML document with its resource key for sorting.
type resourceEntry struct {
	key     resourceKey
	content string
}

// printResourcesBoxed splits multi-doc YAML into individual resources, sorts
// them by kind/namespace/name, and prints each with a box header (same format
// as diff output).
func printResourcesBoxed(data []byte) {
	if len(bytes.TrimSpace(data)) == 0 {
		return
	}

	docs := flux.SplitYAMLText(data)
	var entries []resourceEntry

	for _, doc := range docs {
		trimmed := strings.TrimSpace(doc)
		if trimmed == "" {
			continue
		}

		var meta struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name      string `yaml:"name"`
				Namespace string `yaml:"namespace"`
			} `yaml:"metadata"`
		}
		if err := yaml.Unmarshal([]byte(trimmed), &meta); err != nil {
			continue
		}
		if meta.Kind == "" || meta.Metadata.Name == "" {
			continue
		}

		normalized := reorderYAMLFields([]byte(trimmed))
		converted, err := helm.ConvertJSONInYAMLToYAML(normalized)
		if err != nil || converted == nil {
			continue
		}
		redacted := string(flux.RedactSecrets(converted))

		entries = append(entries, resourceEntry{
			key: resourceKey{
				Kind:      meta.Kind,
				Namespace: meta.Metadata.Namespace,
				Name:      meta.Metadata.Name,
			},
			content: strings.TrimSpace(redacted),
		})
	}

	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i].key, entries[j].key
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})

	for _, e := range entries {
		header := e.key.String()
		border := strings.Repeat("-", len(header)+2)
		fmt.Printf("%s\n %s\n%s\n%s\n\n", border, header, border, e.content)
	}
}

// reorderYAMLFields reorders top-level YAML fields to Kubernetes convention:
// apiVersion, kind, metadata first, then other fields in original order.
// Also strips SOPS metadata from Secret resources.
func reorderYAMLFields(data []byte) []byte {
	if len(data) == 0 {
		return data
	}

	var docs [][]byte
	var currentLines []string

	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "---" {
			if len(currentLines) > 0 {
				docs = append(docs, []byte(strings.Join(currentLines, "\n")))
				currentLines = nil
			}
		} else {
			currentLines = append(currentLines, line)
		}
	}
	if len(currentLines) > 0 {
		docs = append(docs, []byte(strings.Join(currentLines, "\n")))
	}

	var result [][]byte
	for _, doc := range docs {
		trimmed := bytes.TrimSpace(doc)
		if len(trimmed) == 0 {
			continue
		}
		trimmed = stripSOPSFields(trimmed)
		result = append(result, reorderSingleDoc(trimmed))
	}

	return bytes.Join(result, []byte("\n---\n"))
}

// stripSOPSFields removes the top-level "sops:" section from a YAML document.
func stripSOPSFields(data []byte) []byte {
	lines := strings.Split(string(data), "\n")
	var result []string
	skip := false

	for _, line := range lines {
		if len(line) > 0 && line[0] != ' ' && line[0] != '\t' && strings.HasPrefix(line, "sops:") {
			skip = true
			continue
		}
		if skip {
			if len(line) > 0 && (line[0] == ' ' || line[0] == '\t') {
				continue
			}
			skip = false
		}
		result = append(result, line)
	}

	return []byte(strings.Join(result, "\n"))
}

// reorderSingleDoc reorders top-level keys in a single YAML document.
func reorderSingleDoc(doc []byte) []byte {
	lines := strings.Split(string(doc), "\n")

	type section struct {
		key   string
		lines []string
	}
	var sections []section

	for _, line := range lines {
		if len(line) > 0 && line[0] != ' ' && line[0] != '\t' && line[0] != '-' && line[0] != '#' && strings.Contains(line, ":") {
			key := strings.SplitN(line, ":", 2)[0]
			sections = append(sections, section{key: key, lines: []string{line}})
		} else if len(sections) > 0 {
			sections[len(sections)-1].lines = append(sections[len(sections)-1].lines, line)
		} else {
			sections = append(sections, section{key: "", lines: []string{line}})
		}
	}

	priority := []string{"apiVersion", "kind", "metadata"}
	seen := make(map[string]bool)
	var ordered []section

	for _, p := range priority {
		for _, sec := range sections {
			if sec.key == p && !seen[p] {
				ordered = append(ordered, sec)
				seen[p] = true
				break
			}
		}
	}

	for _, sec := range sections {
		if !seen[sec.key] {
			ordered = append(ordered, sec)
			seen[sec.key] = true
		}
	}

	var resultLines []string
	for _, sec := range ordered {
		resultLines = append(resultLines, sec.lines...)
	}

	return []byte(strings.Join(resultLines, "\n"))
}
