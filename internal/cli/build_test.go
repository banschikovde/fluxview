package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/banschikovde/fluxview/internal/flux"
	"github.com/banschikovde/fluxview/internal/kustomize"
)

func TestReorderYAMLFields(t *testing.T) {
	input := []byte(`spec:
  replicas: 1
metadata:
  name: test
apiVersion: v1
kind: ConfigMap
`)
	result := reorderYAMLFields(input)
	resultStr := string(result)

	// apiVersion, kind, metadata should come before spec.
	apiIdx := indexOf(resultStr, "apiVersion:")
	kindIdx := indexOf(resultStr, "kind:")
	metaIdx := indexOf(resultStr, "metadata:")
	specIdx := indexOf(resultStr, "spec:")

	if apiIdx < 0 || kindIdx < 0 || metaIdx < 0 || specIdx < 0 {
		t.Fatalf("missing expected fields in output: %s", resultStr)
	}
	if !(apiIdx < kindIdx && kindIdx < metaIdx && metaIdx < specIdx) {
		t.Errorf("expected apiVersion < kind < metadata < spec, got positions: %d %d %d %d", apiIdx, kindIdx, metaIdx, specIdx)
	}
}

func TestReorderYAMLFields_MultiDoc(t *testing.T) {
	input := []byte(`spec:
  a: 1
metadata:
  name: doc1
apiVersion: v1
kind: ConfigMap
---
spec:
  b: 2
metadata:
  name: doc2
apiVersion: v1
kind: ConfigMap
`)
	result := reorderYAMLFields(input)
	resultStr := string(result)

	// Both documents should be reordered.
	if indexOf(resultStr, "apiVersion:") > indexOf(resultStr, "spec:") {
		t.Error("expected apiVersion before spec in multi-doc output")
	}
}

func TestStripSOPSFields(t *testing.T) {
	input := []byte(`apiVersion: v1
kind: Secret
metadata:
  name: test
data:
  password: cGFzcw==
sops:
  mac: ENC[AES256]
  version: 3.8.1
`)
	result := stripSOPSFields(input)
	resultStr := string(result)

	if indexOf(resultStr, "sops:") >= 0 {
		t.Error("expected sops section to be stripped")
	}
	if indexOf(resultStr, "mac:") >= 0 {
		t.Error("expected sops.mac to be stripped")
	}
	if indexOf(resultStr, "password:") < 0 {
		t.Error("expected data.password to be preserved")
	}
}

func TestStripSOPSFields_NoSOPS(t *testing.T) {
	input := []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: test
data:
  key: value
`)
	result := stripSOPSFields(input)
	if string(result) != string(input) {
		t.Errorf("expected unchanged output when no sops section")
	}
}

func TestReorderSingleDoc_ListItem(t *testing.T) {
	// A YAML list item at zero indentation should NOT be treated as a top-level key.
	input := []byte("- name: item1\n- name: item2\n")
	result := reorderSingleDoc(input)
	resultStr := string(result)

	if indexOf(resultStr, "item1") < 0 || indexOf(resultStr, "item2") < 0 {
		t.Errorf("expected list items to be preserved, got: %s", resultStr)
	}
}

func TestFilterKustomizations_ByName(t *testing.T) {
	ks := []flux.Kustomization{
		{Metadata: flux.ObjectMeta{Name: "base", Namespace: "flux-system"}},
		{Metadata: flux.ObjectMeta{Name: "crds", Namespace: "flux-system"}},
		{Metadata: flux.ObjectMeta{Name: "system", Namespace: "flux-system"}},
	}

	tests := []struct {
		name      string
		args      []flux.Kustomization
		filter    string
		wantCount int
		wantName  string
	}{
		{"no filter returns all", ks, "", 3, ""},
		{"filter by name", ks, "base", 1, "base"},
		{"filter nonexistent", ks, "nonexistent", 0, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := filterKustomizations(tt.args, tt.filter)
			if len(result) != tt.wantCount {
				t.Errorf("got %d, want %d", len(result), tt.wantCount)
			}
			if tt.wantName != "" && len(result) > 0 && result[0].Metadata.Name != tt.wantName {
				t.Errorf("got name %q, want %q", result[0].Metadata.Name, tt.wantName)
			}
		})
	}
}

func TestFilterHelmReleases_ByName(t *testing.T) {
	hr := []flux.HelmRelease{
		{Metadata: flux.ObjectMeta{Name: "podinfo", Namespace: "default"}},
		{Metadata: flux.ObjectMeta{Name: "metallb", Namespace: "metallb-system"}},
	}

	tests := []struct {
		name      string
		args      []flux.HelmRelease
		filter    string
		wantCount int
	}{
		{"no filter returns all", hr, "", 2},
		{"filter by name", hr, "podinfo", 1},
		{"filter nonexistent", hr, "xyz", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := filterHelmReleases(tt.args, tt.filter)
			if len(result) != tt.wantCount {
				t.Errorf("got %d, want %d", len(result), tt.wantCount)
			}
		})
	}
}

func TestResolveOCIRepoURL(t *testing.T) {
	ociRepos := []flux.OCIRepository{
		{Metadata: flux.ObjectMeta{Name: "chart-a", Namespace: "ns1"}, Spec: flux.OCIRepositorySpec{URL: "oci://registry.io/chart-a"}},
		{Metadata: flux.ObjectMeta{Name: "chart-b", Namespace: "ns2"}, Spec: flux.OCIRepositorySpec{URL: "oci://registry.io/chart-b"}},
	}

	tests := []struct {
		name    string
		hr      flux.HelmRelease
		wantURL string
		wantVer string
	}{
		{
			"found same namespace",
			flux.HelmRelease{
				Metadata: flux.ObjectMeta{Name: "hr1", Namespace: "ns1"},
				Spec:     flux.HelmReleaseSpec{ChartRef: &flux.ChartRef{Kind: "OCIRepository", Name: "chart-a"}},
			},
			"oci://registry.io/chart-a", "",
		},
		{
			"found cross-namespace",
			flux.HelmRelease{
				Metadata: flux.ObjectMeta{Name: "hr2", Namespace: "ns1"},
				Spec:     flux.HelmReleaseSpec{ChartRef: &flux.ChartRef{Kind: "OCIRepository", Name: "chart-b", Namespace: "ns2"}},
			},
			"oci://registry.io/chart-b", "",
		},
		{
			"not found wrong namespace",
			flux.HelmRelease{
				Metadata: flux.ObjectMeta{Name: "hr3", Namespace: "ns1"},
				Spec:     flux.HelmReleaseSpec{ChartRef: &flux.ChartRef{Kind: "OCIRepository", Name: "chart-b"}},
			},
			"", "",
		},
		{
			"not found wrong name",
			flux.HelmRelease{
				Metadata: flux.ObjectMeta{Name: "hr4", Namespace: "ns1"},
				Spec:     flux.HelmReleaseSpec{ChartRef: &flux.ChartRef{Kind: "OCIRepository", Name: "chart-x"}},
			},
			"", "",
		},
		{
			"nil chartRef",
			flux.HelmRelease{Metadata: flux.ObjectMeta{Name: "hr5", Namespace: "ns1"}},
			"", "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			url, ver := resolveOCIRepoURL(tt.hr, ociRepos)
			if url != tt.wantURL {
				t.Errorf("url = %q, want %q", url, tt.wantURL)
			}
			if ver != tt.wantVer {
				t.Errorf("version = %q, want %q", ver, tt.wantVer)
			}
		})
	}
}

func TestResolveOCIRepoURL_WithRef(t *testing.T) {
	ociRepos := []flux.OCIRepository{
		{
			Metadata: flux.ObjectMeta{Name: "chart-tagged", Namespace: "ns1"},
			Spec: flux.OCIRepositorySpec{
				URL: "oci://registry.io/chart",
				Ref: &flux.OCIRepositoryRef{Tag: "v1.2.3"},
			},
		},
		{
			Metadata: flux.ObjectMeta{Name: "chart-semver", Namespace: "ns1"},
			Spec: flux.OCIRepositorySpec{
				URL: "oci://registry.io/chart",
				Ref: &flux.OCIRepositoryRef{Semver: "^1.0.0"},
			},
		},
		{
			Metadata: flux.ObjectMeta{Name: "chart-digest", Namespace: "ns1"},
			Spec: flux.OCIRepositorySpec{
				URL: "oci://registry.io/chart",
				Ref: &flux.OCIRepositoryRef{Digest: "sha256:abc123"},
			},
		},
		{
			Metadata: flux.ObjectMeta{Name: "chart-both", Namespace: "ns1"},
			Spec: flux.OCIRepositorySpec{
				URL: "oci://registry.io/chart",
				Ref: &flux.OCIRepositoryRef{Tag: "v1.0.0", Semver: "^2.0.0"},
			},
		},
	}

	// Tag only.
	hr := flux.HelmRelease{
		Metadata: flux.ObjectMeta{Name: "hr", Namespace: "ns1"},
		Spec:     flux.HelmReleaseSpec{ChartRef: &flux.ChartRef{Kind: "OCIRepository", Name: "chart-tagged"}},
	}
	ref, ver := resolveOCIRepoURL(hr, ociRepos)
	if ref != "oci://registry.io/chart" {
		t.Errorf("tag: ref = %q, want oci://registry.io/chart", ref)
	}
	if ver != "v1.2.3" {
		t.Errorf("tag: version = %q, want v1.2.3", ver)
	}

	// Semver only.
	hr.Spec.ChartRef.Name = "chart-semver"
	ref, ver = resolveOCIRepoURL(hr, ociRepos)
	if ver != "^1.0.0" {
		t.Errorf("semver: version = %q, want ^1.0.0", ver)
	}

	// Both tag+semver → semver wins (higher priority).
	hr.Spec.ChartRef.Name = "chart-both"
	_, ver = resolveOCIRepoURL(hr, ociRepos)
	if ver != "^2.0.0" {
		t.Errorf("tag+semver: version = %q, want ^2.0.0 (semver > tag)", ver)
	}

	// Digest → URL gets @digest appended, version empty.
	hr.Spec.ChartRef.Name = "chart-digest"
	ref, ver = resolveOCIRepoURL(hr, ociRepos)
	if !strings.Contains(ref, "@sha256:abc123") {
		t.Errorf("digest: ref = %q, want URL with @sha256:abc123", ref)
	}
	if ver != "" {
		t.Errorf("digest: version = %q, want empty", ver)
	}
}

func TestResolveHelmReleasesWithKustomizeNamespaces_InheritsKustomizeNamespace(t *testing.T) {
	clusterPath := t.TempDir()

	overlayDir := filepath.Join(clusterPath, "cert-manager")
	if err := os.MkdirAll(overlayDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	// HelmRelease with NO explicit metadata.namespace — relies entirely on
	// the kustomize overlay's `namespace:` transformer, as is common in
	// base/overlay layouts.
	hrYAML := `apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: cert-manager
spec:
  chart:
    spec:
      chart: cert-manager
`
	if err := os.WriteFile(filepath.Join(overlayDir, "helmrelease.yaml"), []byte(hrYAML), 0o644); err != nil {
		t.Fatalf("WriteFile helmrelease.yaml: %v", err)
	}

	kustYAML := `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: cert-manager
resources:
  - helmrelease.yaml
`
	if err := os.WriteFile(filepath.Join(overlayDir, "kustomization.yaml"), []byte(kustYAML), 0o644); err != nil {
		t.Fatalf("WriteFile kustomization.yaml: %v", err)
	}

	// A second, loose HelmRelease outside any kustomize overlay, with an
	// explicit namespace — should still be found via the raw-parse fallback.
	looseYAML := `apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: podinfo
  namespace: apps
spec:
  chart:
    spec:
      chart: podinfo
`
	if err := os.WriteFile(filepath.Join(clusterPath, "podinfo.yaml"), []byte(looseYAML), 0o644); err != nil {
		t.Fatalf("WriteFile podinfo.yaml: %v", err)
	}

	builder := kustomize.NewBuilder()
	buildCache := make(map[string][]byte)
	helmReleases, err := resolveHelmReleasesWithKustomizeNamespaces(context.Background(), clusterPath, builder, buildCache)
	if err != nil {
		t.Fatalf("resolveHelmReleasesWithKustomizeNamespaces: %v", err)
	}

	var certManagerHR, podinfoHR *flux.HelmRelease
	for i := range helmReleases {
		switch helmReleases[i].Metadata.Name {
		case "cert-manager":
			certManagerHR = &helmReleases[i]
		case "podinfo":
			podinfoHR = &helmReleases[i]
		}
	}

	if certManagerHR == nil {
		t.Fatalf("cert-manager HelmRelease not found in result: %+v", helmReleases)
	}
	if certManagerHR.Metadata.Namespace != "cert-manager" {
		t.Errorf("cert-manager HelmRelease namespace = %q, want %q (inherited from kustomize namespace: transformer)",
			certManagerHR.Metadata.Namespace, "cert-manager")
	}

	if podinfoHR == nil {
		t.Fatalf("podinfo HelmRelease (loose file, no kustomize overlay) not found in result: %+v", helmReleases)
	}
	if podinfoHR.Metadata.Namespace != "apps" {
		t.Errorf("podinfo HelmRelease namespace = %q, want %q (explicit, no transformer involved)",
			podinfoHR.Metadata.Namespace, "apps")
	}

	// filterHelmReleasesByTargetNamespace should now correctly match the
	// kustomize-inherited namespace, reproducing the reported bug fix.
	filtered := filterHelmReleasesByNamespace(helmReleases, "cert-manager")
	if len(filtered) != 1 || filtered[0].Metadata.Name != "cert-manager" {
		t.Errorf("filterHelmReleasesByNamespace(%q) = %+v, want exactly the cert-manager HelmRelease",
			"cert-manager", filtered)
	}
}

func indexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
