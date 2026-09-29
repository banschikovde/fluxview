package inventory

import (
	"strings"
	"testing"
)

func TestDiffComponents(t *testing.T) {
	before := []Component{
		{Source: SourceHelm, Namespace: "monitoring", Name: "vm-operator", Version: "v0.49.0"},
		{Source: SourceCR, Namespace: "monitoring", Name: "vmcluster", Version: "v1.145.0"},
		{Source: SourceManifest, Namespace: "tools", Name: "echo-server", Version: "0.9.1"},
		{Source: SourceHelm, Namespace: "ops", Name: "old-release", Version: "1.0.0"},
	}
	after := []Component{
		{Source: SourceHelm, Namespace: "monitoring", Name: "vm-operator", Version: "v0.50.1"}, // updated
		{Source: SourceCR, Namespace: "monitoring", Name: "vmcluster", Version: "v1.145.0"},    // unchanged
		{Source: SourceManifest, Namespace: "tools", Name: "echo-server", Version: "0.9.2"},    // updated
		{Source: SourceHelm, Namespace: "monitoring", Name: "new-release", Version: "2.0.0"},   // added
	}

	out := DiffComponents(before, after, []string{"namespace", "name"})

	byKey := map[string]Component{}
	for _, c := range out {
		byKey[c.Source+"/"+c.Namespace+"/"+c.Name] = c
	}

	if c := byKey["helm/monitoring/vm-operator"]; c.Change != "~ v0.49.0 → v0.50.1" {
		t.Errorf("updated change = %q", c.Change)
	}
	if c := byKey["cr/monitoring/vmcluster"]; c.Change != "" {
		t.Errorf("unchanged component must have empty change, got %q", c.Change)
	}
	if c := byKey["manifest/tools/echo-server"]; c.Change != "~ 0.9.1 → 0.9.2" {
		t.Errorf("updated change = %q", c.Change)
	}
	if c := byKey["helm/monitoring/new-release"]; c.Change != "+" {
		t.Errorf("added change = %q", c.Change)
	}
	if c := byKey["helm/ops/old-release"]; c.Change != "-" || c.Version != "1.0.0" {
		t.Errorf("removed change = %q, version = %q", c.Change, c.Version)
	}
	if len(out) != 5 {
		t.Errorf("got %d rows, want 5 (4 current + 1 removed)", len(out))
	}
	if !HasChanges(out) {
		t.Error("HasChanges must be true")
	}
	if HasChanges(after) {
		t.Error("HasChanges must be false for untouched components")
	}
}

func TestDiffComponents_IdentityIncludesKind(t *testing.T) {
	// One name, two kinds — the VMCluster/VMAuth and VLAgent/VMAuth pattern
	// the KIND column exists for. Each kind pairs with its own row across
	// revisions: no cross-kind version markers, zero diff stays zero.
	before := []Component{
		{Source: SourceCR, Namespace: "victoria-logs", Name: "vlogs", Kind: "VLAgent", Version: "v1.52.0"},
		{Source: SourceCR, Namespace: "victoria-logs", Name: "vlogs", Kind: "VMAuth", Version: "v1.152.0"},
	}
	after := []Component{
		{Source: SourceCR, Namespace: "victoria-logs", Name: "vlogs", Kind: "VLAgent", Version: "v1.52.0"},
		{Source: SourceCR, Namespace: "victoria-logs", Name: "vlogs", Kind: "VMAuth", Version: "v1.152.0"},
	}
	out := DiffComponents(before, after, nil)
	if len(out) != 2 || HasChanges(out) {
		t.Errorf("identical revisions must produce a zero diff: %+v", out)
	}

	// A version bump of ONE kind marks only that row.
	after[0].Version = "v1.53.0"
	out = DiffComponents(before, after, nil)
	markers := 0
	for _, c := range out {
		switch {
		case c.Kind == "VLAgent" && c.Change != "~ v1.52.0 → v1.53.0":
			t.Errorf("VLAgent marker = %q", c.Change)
		case c.Kind == "VMAuth" && c.Change != "":
			t.Errorf("VMAuth must stay unchanged, got %q", c.Change)
		}
		if c.Change != "" {
			markers++
		}
	}
	if markers != 1 {
		t.Errorf("exactly one marker expected, got %d", markers)
	}

	// A kind change is an honest remove+add pair, not an update.
	replaced := []Component{{Source: SourceManifest, Namespace: "tools", Name: "app", Kind: "StatefulSet", Version: "1.0"}}
	out = DiffComponents([]Component{{Source: SourceManifest, Namespace: "tools", Name: "app", Kind: "Deployment", Version: "1.0"}}, replaced, nil)
	if len(out) != 2 {
		t.Fatalf("kind change must yield two rows, got %d: %+v", len(out), out)
	}
	changes := map[string]string{}
	for _, c := range out {
		changes[c.Kind] = c.Change
	}
	if changes["Deployment"] != "-" || changes["StatefulSet"] != "+" {
		t.Errorf("kind change = -/+ pair, got %+v", changes)
	}
}

func TestMarkdown(t *testing.T) {
	components := []Component{
		{
			Source: SourceHelm, Namespace: "cert-manager", Name: "cert-manager", Software: "cert-manager",
			Version: "v1.16.2", Chart: &ChartInfo{Name: "cert-manager", Version: "1.16.2"},
		},
		{
			Source: SourceCR, Kind: "VMCluster", Namespace: "victoria-metrics", Name: "vmcluster", Software: "VictoriaMetrics cluster",
			Version: "v1.146.0-cluster",
		},
	}
	out := Markdown(components, RenderOptions{})

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	want := []string{
		"## Helm releases (1)",
		"",
		"| Namespace | Name | Software | Version | Chart | Images |",
		"| --- | --- | --- | --- | --- | --- |",
		"| cert-manager | cert-manager | cert-manager | v1.16.2 | 1.16.2 | none |",
		"",
		"## Operator custom resources (1)",
		"",
		"| Namespace | Name | Kind | Parent | Software | Version | Images |",
		"| --- | --- | --- | --- | --- | --- | --- |",
		"| victoria-metrics | vmcluster | VMCluster | - | VictoriaMetrics cluster | v1.146.0-cluster | none |",
	}
	if len(lines) != len(want) {
		t.Fatalf("got %d lines, want %d:\n%s", len(lines), len(want), out)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("line %d:\n got %q\nwant %q", i, lines[i], want[i])
		}
	}
}

func TestMarkdown_EscapesPipes(t *testing.T) {
	components := []Component{
		{Source: SourceCR, Namespace: "tools", Name: "a|b", Software: "x|y", Version: "1|0"},
	}
	out := Markdown(components, RenderOptions{})
	if strings.Contains(out, "| a|b ") || strings.Contains(out, "| x|y ") {
		t.Errorf("unescaped pipes break the table:\n%s", out)
	}
	if !strings.Contains(out, `a\|b`) {
		t.Errorf("pipes must be escaped:\n%s", out)
	}
}

func TestMarkdown_Changes(t *testing.T) {
	components := []Component{
		{
			Source: SourceHelm, Namespace: "monitoring", Name: "vm-operator", Software: "victoria-metrics-operator",
			Version: "v0.50.1",
			Change:  "~ v0.49.0 → v0.50.1",
		},
	}
	out := Markdown(components, RenderOptions{Changes: true})
	if !strings.Contains(out, "| Change | Namespace |") {
		t.Errorf("change column must lead the block table:\n%s", out)
	}
	if !strings.Contains(out, "~ v0.49.0 → v0.50.1") {
		t.Errorf("change marker missing:\n%s", out)
	}
}

func TestTable_Changes(t *testing.T) {
	components := []Component{
		{
			Source: SourceCR, Namespace: "monitoring", Name: "vmcluster", Version: "v1.146.0", Kind: "VMCluster",
			Operator: "victoria-metrics-operator", Change: "+",
		},
	}
	out := Table(components, RenderOptions{Headers: true, Changes: true})
	if !strings.Contains(out, "CHANGE") {
		t.Errorf("change header missing:\n%s", out)
	}
	if !strings.Contains(out, "+") {
		t.Errorf("change marker missing:\n%s", out)
	}

	// Unchanged rows render "=".
	components[0].Change = ""
	out = Table(components, RenderOptions{Headers: true, Changes: true})
	if !strings.Contains(out, "=") {
		t.Errorf("unchanged marker missing:\n%s", out)
	}
}
