package inventory

import (
	"fmt"
	"slices"
	"strings"
)

// FilterOptions controls which components are shown and in what order.
type FilterOptions struct {
	// Namespace keeps only components in the given namespace ("" = all).
	Namespace string
	// Sources keeps only these source kinds (nil/empty = all).
	Sources map[string]bool
	// Sort lists the sort keys in priority order (name, namespace, source,
	// version). Tie-breakers are always appended for determinism.
	Sort []string
}

// DefaultSort is the default --sort value.
const DefaultSort = "source,namespace,name"

// ParseSort validates a comma-separated sort spec and returns the keys.
func ParseSort(spec string) ([]string, error) {
	if strings.TrimSpace(spec) == "" {
		return nil, fmt.Errorf("empty sort spec")
	}
	valid := map[string]bool{"name": true, "namespace": true, "source": true, "version": true}
	var keys []string
	seen := map[string]bool{}
	for _, key := range strings.Split(spec, ",") {
		key = strings.TrimSpace(key)
		if !valid[key] {
			return nil, fmt.Errorf("unknown sort key %q (use name, namespace, source or version)", key)
		}
		if seen[key] {
			return nil, fmt.Errorf("duplicate sort key %q", key)
		}
		seen[key] = true
		keys = append(keys, key)
	}
	return keys, nil
}

// Filter applies namespace and source filters. The input is not modified.
func Filter(components []Component, opts FilterOptions) []Component {
	out := make([]Component, 0, len(components))
	for _, c := range components {
		if opts.Namespace != "" && c.Namespace != opts.Namespace {
			continue
		}
		if len(opts.Sources) > 0 && !opts.Sources[c.Source] {
			continue
		}
		out = append(out, c)
	}
	return out
}

// Sort orders components by the given keys. The remaining canonical keys are
// always appended as tie-breakers, making the order total and the output
// byte-for-byte deterministic for equal input.
func Sort(components []Component, keys []string) {
	keySet := map[string]bool{}
	for _, k := range keys {
		keySet[k] = true
	}
	effective := append([]string{}, keys...)
	for _, k := range []string{"source", "namespace", "name", "kind"} {
		if !keySet[k] {
			effective = append(effective, k)
		}
	}
	compare := func(a, b Component) int {
		for _, k := range effective {
			var av, bv string
			switch k {
			case "name":
				av, bv = a.Name, b.Name
			case "namespace":
				av, bv = a.Namespace, b.Namespace
			case "source":
				av, bv = a.Source, b.Source
			case "version":
				av, bv = a.Version, b.Version
			case "kind":
				av, bv = a.Kind, b.Kind
			}
			if av != bv {
				return strings.Compare(av, bv)
			}
		}
		return 0
	}
	slices.SortStableFunc(components, compare)
}

// dash replaces empty values with the table placeholder.
func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// RenderOptions controls the table rendering.
type RenderOptions struct {
	// Headers enables the human layout: per-source section titles, header
	// rows and the summary line. Disabled (--no-headers) yields flat data
	// rows only, with the SOURCE column so each row is self-describing.
	Headers bool
	// Changes adds the leading CHANGE column (--branch-orig).
	Changes bool
}

// noneCell is the IMAGES column placeholder when no image is known —
// distinct from the generic dash, which marks fields a component does not
// have at all.
func noneCell(c Component) string {
	if len(c.Images) == 0 {
		return "none"
	}
	return strings.Join(c.Images, ", ")
}

// sectionTitles labels each source block of the human layout.
var sectionTitles = map[string]string{
	SourceHelm:     "Helm releases",
	SourceCR:       "Operator custom resources",
	SourceManifest: "Plain manifests",
	SourceCRD:      "CRD groups",
}

// column is one display column of a section.
type column struct {
	header string // table header; markdown capitalizes the first letter
	cell   func(c Component) string
}

// changeColumn is the leading --branch-orig column, shared by all sections.
var changeColumn = column{"CHANGE", func(c Component) string { return changeCell(c) }}

// sectionColumns lists the columns of one source section. The IMAGES
// column is always present for helm (rendered charts), manifest (own
// manifests) and cr (rule extraction); missing images render as "none".
func sectionColumns(source string) []column {
	chartCell := func(c Component) string {
		if c.Chart != nil {
			return dash(c.Chart.Version)
		}
		return "-"
	}
	switch source {
	case SourceHelm:
		return []column{
			{"NAMESPACE", func(c Component) string { return dash(c.Namespace) }},
			{"NAME", func(c Component) string { return dash(c.Name) }},
			{"SOFTWARE", func(c Component) string { return dash(c.Software) }},
			{"VERSION", func(c Component) string { return dash(c.Version) }},
			{"CHART", chartCell},
			{"IMAGES", noneCell},
		}
	case SourceCR:
		return []column{
			{"NAMESPACE", func(c Component) string { return dash(c.Namespace) }},
			{"NAME", func(c Component) string { return dash(c.Name) }},
			{"KIND", func(c Component) string { return dash(c.Kind) }},
			{"PARENT", func(c Component) string { return dash(c.Parent) }},
			{"SOFTWARE", func(c Component) string { return dash(c.Software) }},
			{"VERSION", func(c Component) string { return dash(c.Version) }},
			{"IMAGES", noneCell},
		}
	case SourceCRD:
		// A CRD group is cluster-scoped and has no software of its own —
		// only the group name (with its CRD count) and the version matter.
		return []column{
			{"NAME", func(c Component) string { return dash(c.Name) }},
			{"VERSION", func(c Component) string { return dash(c.Version) }},
		}
	default: // manifest
		return []column{
			{"NAMESPACE", func(c Component) string { return dash(c.Namespace) }},
			{"NAME", func(c Component) string { return dash(c.Name) }},
			{"SOFTWARE", func(c Component) string { return dash(c.Software) }},
			{"VERSION", func(c Component) string { return dash(c.Version) }},
			{"IMAGES", noneCell},
		}
	}
}

// cellsFor renders a component into the section's cells, prepending the
// CHANGE column when requested.
func cellsFor(source string, changes bool, c Component) []string {
	var cells []string
	if changes {
		cells = append(cells, changeColumn.cell(c))
	}
	for _, col := range sectionColumns(source) {
		cells = append(cells, col.cell(c))
	}
	return cells
}

// headersFor builds the section's header cells, uppercase for the table
// format. withChange prepends the CHANGE column header.
func headersFor(source string, changes bool) []string {
	var headers []string
	if changes {
		headers = append(headers, changeColumn.header)
	}
	for _, col := range sectionColumns(source) {
		headers = append(headers, col.header)
	}
	return headers
}

// headersForMarkdown capitalizes the first letter of each table header.
func headersForMarkdown(headers []string) []string {
	out := make([]string, len(headers))
	for i, h := range headers {
		if h == "" {
			out[i] = h
			continue
		}
		out[i] = strings.ToUpper(h[:1]) + strings.ToLower(h[1:])
	}
	return out
}

// changeCell renders the CHANGE column value for a component.
func changeCell(c Component) string {
	if c.Change == "" {
		return "="
	}
	return c.Change
}

// padRow renders one row padded to the column widths; the last column
// carries no padding, so lines have no trailing spaces.
func padRow(cells []string, widths []int) string {
	var b strings.Builder
	for i, cell := range cells {
		if i == len(cells)-1 {
			b.WriteString(cell)
			break
		}
		b.WriteString(cell)
		b.WriteString(strings.Repeat(" ", widths[i]-len([]rune(cell))+2))
	}
	return b.String()
}

// columnWidths computes per-column widths over the given rows.
func columnWidths(rows [][]string) []int {
	widths := make([]int, len(rows[0]))
	for _, row := range rows {
		for i, cell := range row {
			if w := len([]rune(cell)); w > widths[i] {
				widths[i] = w
			}
		}
	}
	return widths
}

// Table renders components as fixed-width text (the -o table format). With
// headers enabled the output is sectioned per source — a title line with the
// component count, then a header row and the block's rows; a summary line
// closes the output. Without headers it is flat data rows, SOURCE column
// included, for scripts. The output is byte-for-byte deterministic for
// equal input.
func Table(components []Component, opts RenderOptions) string {
	if !opts.Headers {
		if len(components) == 0 {
			return ""
		}
		// Fixed schema across all rows — scripts parse by position.
		flat := []column{
			{"SOURCE", func(c Component) string { return dash(c.Source) }},
			{"NAMESPACE", func(c Component) string { return dash(c.Namespace) }},
			{"NAME", func(c Component) string { return dash(c.Name) }},
			{"PARENT", func(c Component) string { return dash(c.Parent) }},
			{"SOFTWARE", func(c Component) string { return dash(c.Software) }},
			{"VERSION", func(c Component) string { return dash(c.Version) }},
			{"CHART", func(c Component) string {
				if c.Chart != nil {
					return dash(c.Chart.Version)
				}
				return "-"
			}},
			{"IMAGES", noneCell},
		}
		lines := make([]string, 0, len(components))
		for _, c := range components {
			var cells []string
			if opts.Changes {
				cells = append(cells, changeCell(c))
			}
			for _, col := range flat {
				cells = append(cells, col.cell(c))
			}
			lines = append(lines, strings.Join(cells, "  "))
		}
		return strings.Join(lines, "\n") + "\n"
	}

	var b strings.Builder
	for _, source := range CanonicalSources {
		var block []Component
		for _, c := range components {
			if c.Source == source {
				block = append(block, c)
			}
		}
		if len(block) == 0 {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(fmt.Sprintf("%s (%d)\n", sectionTitles[source], len(block)))

		rows := [][]string{headersFor(source, opts.Changes)}
		for _, c := range block {
			rows = append(rows, cellsFor(source, opts.Changes, c))
		}
		widths := columnWidths(rows)
		for _, row := range rows {
			b.WriteString(padRow(row, widths))
			b.WriteByte('\n')
		}
	}

	if len(components) == 0 {
		b.WriteString("0 components\n")
	} else {
		b.WriteString(summaryLine(components))
		b.WriteByte('\n')
	}
	return b.String()
}

// escapeMD escapes a Markdown table cell value: pipes break the table, raw
// newlines end the row (acceptance: -o markdown must render in GitHub/GitLab).
func escapeMD(s string) string {
	s = strings.ReplaceAll(s, "|", `\|`)
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

// Markdown renders components as GFM (the -o markdown format) that can be
// pasted into an MR or wiki as is: one section per source, titled and
// counted, each with its own table. No summary line: Markdown tables carry
// one row per component only.
func Markdown(components []Component, opts RenderOptions) string {
	var b strings.Builder
	for _, source := range CanonicalSources {
		var block []Component
		for _, c := range components {
			if c.Source == source {
				block = append(block, c)
			}
		}
		if len(block) == 0 {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(fmt.Sprintf("## %s (%d)\n\n", sectionTitles[source], len(block)))

		headers := headersForMarkdown(headersFor(source, opts.Changes))
		b.WriteString("| " + strings.Join(headers, " | ") + " |\n")
		seps := make([]string, len(headers))
		for i := range seps {
			seps[i] = "---"
		}
		b.WriteString("| " + strings.Join(seps, " | ") + " |\n")

		for _, c := range block {
			cells := cellsFor(source, opts.Changes, c)
			for i := range cells {
				cells[i] = escapeMD(cells[i])
			}
			b.WriteString("| " + strings.Join(cells, " | ") + " |\n")
		}
	}
	return b.String()
}

// summaryLine renders the totals line: component count per source plus the
// warning count. Sources follow CanonicalSources order; zero-count sources
// are omitted.
func summaryLine(components []Component) string {
	counts := map[string]int{}
	warnings := 0
	for _, c := range components {
		counts[c.Source]++
		warnings += len(c.Warnings)
	}
	parts := make([]string, 0, len(CanonicalSources))
	for _, source := range CanonicalSources {
		if counts[source] > 0 {
			parts = append(parts, fmt.Sprintf("%s: %d", source, counts[source]))
		}
	}
	count := fmt.Sprintf("%d components", len(components))
	if len(components) == 1 {
		count = "1 component"
	}
	var line string
	if len(parts) > 0 {
		line = fmt.Sprintf("%s (%s)", count, strings.Join(parts, ", "))
	} else {
		line = count
	}
	if warnings > 0 {
		line += fmt.Sprintf("; %d warning", warnings)
		if warnings > 1 {
			line += "s"
		}
	}
	return line
}
