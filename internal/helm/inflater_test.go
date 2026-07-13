package helm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/banschikovde/fluxview/internal/flux"
)

// writeTestChart creates a minimal local Helm chart under dir that contains a
// namespaced template (Deployment) and a CRD (in crds/). The Helm SDK renders it
// from disk, so no network access is needed.
func writeTestChart(t *testing.T, dir string) {
	t.Helper()
	write := func(path, content string) {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	write(filepath.Join(dir, "Chart.yaml"), `apiVersion: v2
name: testchart
version: 1.0.0
`)
	write(filepath.Join(dir, "values.yaml"), "")
	write(filepath.Join(dir, "templates", "dep.yaml"), `apiVersion: apps/v1
kind: Deployment
metadata:
  name: app
spec:
  replicas: 1
`)
	write(filepath.Join(dir, "crds", "custom.yaml"), `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: widgets.example.com
spec:
  group: example.com
  scope: Namespaced
  names:
    kind: Widget
`)
}

// TestInflateHelmRelease_CRDSkip verifies that spec.install.crds: Skip excludes
// the chart's CRDs from the rendered output, while the chart's workload
// templates are still rendered.
func TestInflateHelmRelease_CRDSkip(t *testing.T) {
	chartDir := filepath.Join(t.TempDir(), "testchart")
	writeTestChart(t, chartDir)

	inflater, err := NewInflater()
	if err != nil {
		t.Fatalf("NewInflater: %v", err)
	}

	hr := flux.HelmRelease{
		Metadata: flux.ObjectMeta{Name: "app", Namespace: "test"},
		Spec: flux.HelmReleaseSpec{
			Chart:   flux.HelmReleaseChart{Spec: flux.HelmReleaseChartSpec{Chart: chartDir}},
			Install: &flux.InstallSpec{CRDs: "Skip"},
		},
	}

	out, err := inflater.InflateHelmRelease(context.Background(), hr, "", "", "", nil, nil, "")
	if err != nil {
		t.Fatalf("InflateHelmRelease: %v", err)
	}
	rendered := string(out)

	if strings.Contains(rendered, "CustomResourceDefinition") {
		t.Errorf("CRD should be excluded when install.crds=Skip:\n%s", rendered)
	}
	if strings.Contains(rendered, "widgets.example.com") {
		t.Errorf("CRD name should be absent when install.crds=Skip:\n%s", rendered)
	}
	if !strings.Contains(rendered, "kind: Deployment") {
		t.Errorf("workload template should still render when install.crds=Skip:\n%s", rendered)
	}
}

// TestInflateHelmRelease_CRDDefault verifies the default behavior is unchanged:
// when spec.install is absent (or crds is not Skip) the chart's CRDs ARE included.
// Regression guard for the previously hardcoded IncludeCRDs=true.
func TestInflateHelmRelease_CRDDefault(t *testing.T) {
	chartDir := filepath.Join(t.TempDir(), "testchart")
	writeTestChart(t, chartDir)

	inflater, err := NewInflater()
	if err != nil {
		t.Fatalf("NewInflater: %v", err)
	}

	cases := []struct {
		name    string
		install *flux.InstallSpec
	}{
		{"no install field", nil},
		{"createReplace", &flux.InstallSpec{CRDs: "CreateReplace"}},
		{"create", &flux.InstallSpec{CRDs: "Create"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hr := flux.HelmRelease{
				Metadata: flux.ObjectMeta{Name: "app", Namespace: "test"},
				Spec: flux.HelmReleaseSpec{
					Chart:   flux.HelmReleaseChart{Spec: flux.HelmReleaseChartSpec{Chart: chartDir}},
					Install: c.install,
				},
			}

			out, err := inflater.InflateHelmRelease(context.Background(), hr, "", "", "", nil, nil, "")
			if err != nil {
				t.Fatalf("InflateHelmRelease: %v", err)
			}
			rendered := string(out)

			if !strings.Contains(rendered, "CustomResourceDefinition") {
				t.Errorf("CRD should be included by default (install=%v):\n%s", c.install, rendered)
			}
		})
	}
}

// TestInflateHelmRelease_ReleaseName verifies that spec.releaseName overrides
// the Helm release name ({{ .Release.Name }} in templates), falling back to
// metadata.name when unset.
func TestInflateHelmRelease_ReleaseName(t *testing.T) {
	chartDir := filepath.Join(t.TempDir(), "testchart")
	// Chart with a label derived from the release name.
	write := func(path, content string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	write(filepath.Join(chartDir, "Chart.yaml"), `apiVersion: v2
name: testchart
version: 1.0.0
`)
	write(filepath.Join(chartDir, "values.yaml"), "")
	write(filepath.Join(chartDir, "templates", "cm.yaml"), `apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Release.Name }}-config
data:
  release: {{ .Release.Name }}
`)

	inflater, err := NewInflater()
	if err != nil {
		t.Fatalf("NewInflater: %v", err)
	}

	t.Run("explicit releaseName", func(t *testing.T) {
		hr := flux.HelmRelease{
			Metadata: flux.ObjectMeta{Name: "my-hr", Namespace: "test"},
			Spec: flux.HelmReleaseSpec{
				Chart:       flux.HelmReleaseChart{Spec: flux.HelmReleaseChartSpec{Chart: chartDir}},
				ReleaseName: "custom-release",
			},
		}
		out, err := inflater.InflateHelmRelease(context.Background(), hr, "", "", "", nil, nil, "")
		if err != nil {
			t.Fatalf("InflateHelmRelease: %v", err)
		}
		rendered := string(out)
		if !strings.Contains(rendered, "custom-release-config") {
			t.Errorf("expected resource named after releaseName 'custom-release':\n%s", rendered)
		}
		if strings.Contains(rendered, "my-hr-config") {
			t.Errorf("resource should use releaseName, not metadata.name:\n%s", rendered)
		}
	})

	t.Run("defaults to metadata.name", func(t *testing.T) {
		hr := flux.HelmRelease{
			Metadata: flux.ObjectMeta{Name: "my-hr", Namespace: "test"},
			Spec: flux.HelmReleaseSpec{
				Chart: flux.HelmReleaseChart{Spec: flux.HelmReleaseChartSpec{Chart: chartDir}},
			},
		}
		out, err := inflater.InflateHelmRelease(context.Background(), hr, "", "", "", nil, nil, "")
		if err != nil {
			t.Fatalf("InflateHelmRelease: %v", err)
		}
		rendered := string(out)
		if !strings.Contains(rendered, "my-hr-config") {
			t.Errorf("expected resource named after metadata.name 'my-hr' as fallback:\n%s", rendered)
		}
	})
}
