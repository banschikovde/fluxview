package inventory

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseSort(t *testing.T) {
	cases := []struct {
		name    string
		spec    string
		want    []string
		wantErr bool
	}{
		{"default keys", "source,namespace,name", []string{"source", "namespace", "name"}, false},
		{"spaces around keys", " name , version ", []string{"name", "version"}, false},
		{"single key", "version", []string{"version"}, false},
		{"unknown key", "name,size", nil, true},
		{"duplicate key", "name,name", nil, true},
		{"empty", "", nil, true},
		{"empty items", "name,,version", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseSort(tc.spec)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseSort(%q) = %v, want error", tc.spec, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseSort(%q): %v", tc.spec, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ParseSort(%q) = %v, want %v", tc.spec, got, tc.want)
			}
		})
	}
}

func TestFilter(t *testing.T) {
	components := []Component{
		{Source: SourceHelm, Namespace: "a", Name: "x"},
		{Source: SourceCR, Namespace: "b", Name: "y"},
		{Source: SourceManifest, Namespace: "a", Name: "z"},
	}

	got := Filter(components, FilterOptions{Namespace: "a"})
	if len(got) != 2 {
		t.Errorf("namespace filter: got %d components, want 2", len(got))
	}
	for _, c := range got {
		if c.Namespace != "a" {
			t.Errorf("namespace filter leaked %q", c.Namespace)
		}
	}

	got = Filter(components, FilterOptions{Sources: map[string]bool{SourceCR: true}})
	if len(got) != 1 || got[0].Name != "y" {
		t.Errorf("source filter: got %+v, want only y", got)
	}

	got = Filter(components, FilterOptions{})
	if len(got) != 3 {
		t.Errorf("empty filter: got %d components, want all 3", len(got))
	}

	if len(components) != 3 {
		t.Errorf("Filter must not modify its input")
	}
}

func TestSort_Deterministic(t *testing.T) {
	components := []Component{
		{Source: SourceManifest, Namespace: "b", Name: "b", Kind: "Deployment"},
		{Source: SourceHelm, Namespace: "b", Name: "a"},
		{Source: SourceHelm, Namespace: "a", Name: "z"},
		{Source: SourceHelm, Namespace: "a", Name: "z", Kind: "StatefulSet"},
		{Source: SourceCR, Namespace: "a", Name: "m"},
	}
	want := []string{
		"a/m", // cr sorts before helm
		"a/z", // StatefulSet before empty kind ("S" < "")
		"a/z",
		"b/a",
		"b/b",
	}

	Sort(components, []string{"namespace", "name"})
	for i, c := range components {
		got := c.Namespace + "/" + c.Name
		if got != want[i] {
			t.Errorf("row %d = %s, want %s", i, got, want[i])
		}
	}
}

func TestTable_HeadersAndSummary(t *testing.T) {
	components := []Component{
		{
			Source: SourceHelm, Kind: "HelmRelease", Namespace: "cert-manager",
			Name: "cert-manager", Software: "cert-manager", Version: "v1.16.2",
			Chart: &ChartInfo{Name: "cert-manager", Version: "1.16.2"},
		},
		{
			Source: SourceCR, Kind: "VMCluster", Namespace: "victoria-metrics",
			Name: "vmcluster", Software: "VictoriaMetrics cluster", Version: "v1.146.0-cluster",
			Warnings: []string{"operator default version"},
		},
	}
	out := Table(components, RenderOptions{Headers: true})

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	// 2 sections × (title + header + row) + blank separators + summary.
	want := []string{
		"Helm releases (1)",
		"NAMESPACE     NAME          SOFTWARE      VERSION  CHART   IMAGES",
		"cert-manager  cert-manager  cert-manager  v1.16.2  1.16.2  none",
		"",
		"Operator custom resources (1)",
		"NAMESPACE         NAME       KIND       PARENT  SOFTWARE                 VERSION           IMAGES",
		"victoria-metrics  vmcluster  VMCluster  -       VictoriaMetrics cluster  v1.146.0-cluster  none",
		"2 components (helm: 1, cr: 1); 1 warning",
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

func TestTable_NoHeaders(t *testing.T) {
	components := []Component{
		{Source: SourceManifest, Namespace: "tools", Name: "echo-server", Software: "echo-server", Version: "0.9.2"},
	}
	out := Table(components, RenderOptions{})
	if strings.Contains(out, "NAMESPACE") || strings.Contains(out, "manifests") {
		t.Errorf("no-headers output must carry no titles or header rows:\n%s", out)
	}
	if strings.Contains(out, "components") {
		t.Errorf("no-headers output must not contain the summary line:\n%s", out)
	}
	// Flat rows keep the SOURCE column so each row is self-describing.
	if out != "manifest  tools  echo-server  -  echo-server  0.9.2  -  none\n" {
		t.Errorf("no-headers row mismatch:\n%q", out)
	}
}

func TestTable_Empty(t *testing.T) {
	out := Table(nil, RenderOptions{Headers: true})
	if out != "0 components\n" {
		t.Errorf("empty table mismatch:\n%q", out)
	}
	out = Table(nil, RenderOptions{})
	if out != "" {
		t.Errorf("empty no-headers table must be empty, got %q", out)
	}
}

func TestTable_MissingValuesAndSingular(t *testing.T) {
	components := []Component{
		{Source: SourceHelm, Namespace: "ops", Name: "bucket-chart", Warnings: []string{"a", "b"}},
	}
	out := Table(components, RenderOptions{Headers: true})
	if !strings.Contains(out, "ops        bucket-chart  -         -        -      none") {
		t.Errorf("missing values render as dashes; a missing image as none:\n%s", out)
	}
	if !strings.HasSuffix(strings.TrimRight(out, "\n"), "1 component (helm: 1); 2 warnings") {
		t.Errorf("singular summary mismatch:\n%s", out)
	}
}

func TestTable_Deterministic(t *testing.T) {
	components := []Component{
		{Source: SourceCR, Namespace: "z", Name: "last", Software: "s", Version: "2"},
		{Source: SourceHelm, Namespace: "a", Name: "first", Software: "long software name", Version: "1", Chart: &ChartInfo{Version: "1.0"}},
	}
	first := Table(components, RenderOptions{Headers: true})
	second := Table(components, RenderOptions{Headers: true})
	if first != second {
		t.Errorf("table output is not deterministic")
	}
}
