package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/banschikovde/fluxview/internal/flux"
	"github.com/banschikovde/fluxview/internal/helm"
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

// --- Tests for unified HelmRelease discovery (Steps 1-5) ---

// writeHelper writes a file under dir, creating the directory if needed.
func writeHelper(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", name, err)
	}
}

// captureStderr temporarily redirects os.Stderr, runs f, and returns what was written.
func captureStderr(f func()) string {
	old := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	f()
	w.Close()
	os.Stderr = old
	var buf bytes.Buffer
	io.Copy(&buf, r)
	return buf.String()
}

// Test 1+2: A HelmRelease in a shared base outside clusterPath, pulled in
// via Flux Kustomization.spec.path, is discovered through buildAllKustomizations.
// Covers both build ks (extracts HR from output) and build hr (primary path).
func TestBuildAllKustomizations_HelmReleaseInSharedBase(t *testing.T) {
	repoRoot := t.TempDir()

	// Shared base outside clusterPath.
	baseDir := filepath.Join(repoRoot, "apps", "base")
	writeHelper(t, baseDir, "helmrelease.yaml", `apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: podinfo
  namespace: apps
spec:
  chart:
    spec:
      chart: podinfo
      version: "6.0.0"
      sourceRef:
        kind: HelmRepository
        name: podinfo
        namespace: apps
`)
	writeHelper(t, baseDir, "kustomization.yaml", `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - helmrelease.yaml
`)

	// Flux Kustomization in clusterPath pointing to the shared base.
	clusterPath := filepath.Join(repoRoot, "clusters", "test")
	writeHelper(t, clusterPath, "base.yaml", `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: base
  namespace: flux-system
spec:
  path: ../../apps/base
  sourceRef:
    kind: GitRepository
    name: flux-system
`)

	ctx := context.Background()
	kustomizations, err := flux.NewParser(clusterPath).ParseKustomizations(ctx)
	if err != nil {
		t.Fatalf("ParseKustomizations: %v", err)
	}

	builder := kustomize.NewBuilder()
	buildCache := make(map[string][]byte)
	output, err := buildKSContent(ctx, builder, kustomizations, repoRoot, clusterPath, nil, true, buildCache)
	if err != nil {
		t.Fatalf("buildKSContent: %v", err)
	}

	helmReleases, _ := flux.ParseHelmReleasesFromBytes(output)
	if len(helmReleases) != 1 {
		t.Fatalf("expected 1 HelmRelease from shared base, got %d: %+v", len(helmReleases), helmReleases)
	}
	if helmReleases[0].Metadata.Name != "podinfo" {
		t.Errorf("name = %q, want %q", helmReleases[0].Metadata.Name, "podinfo")
	}
	if helmReleases[0].Metadata.Namespace != "apps" {
		t.Errorf("namespace = %q, want %q", helmReleases[0].Metadata.Namespace, "apps")
	}
}

// Test 3 (regression): A HelmRelease directly in clusterPath with no Flux
// Kustomization at all must still be found via the fallback scanner.
func TestResolveHelmReleasesWithKustomizeNamespaces_RawFileOnly(t *testing.T) {
	clusterPath := t.TempDir()
	writeHelper(t, clusterPath, "podinfo.yaml", `apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: podinfo
  namespace: apps
spec:
  chart:
    spec:
      chart: podinfo
`)

	builder := kustomize.NewBuilder()
	buildCache := make(map[string][]byte)
	helmReleases, err := resolveHelmReleasesWithKustomizeNamespaces(context.Background(), clusterPath, builder, buildCache)
	if err != nil {
		t.Fatalf("resolveHelmReleasesWithKustomizeNamespaces: %v", err)
	}
	if len(helmReleases) != 1 {
		t.Fatalf("expected 1 raw HelmRelease, got %d: %+v", len(helmReleases), helmReleases)
	}
	if helmReleases[0].Metadata.Name != "podinfo" {
		t.Errorf("name = %q, want %q", helmReleases[0].Metadata.Name, "podinfo")
	}
}

// Test 4a: dedupHelmReleases — primary takes precedence on collision.
func TestDedupHelmReleases_PrimaryPrecedence(t *testing.T) {
	primary := []flux.HelmRelease{
		{Metadata: flux.ObjectMeta{Name: "podinfo", Namespace: "apps"}, Spec: flux.HelmReleaseSpec{Chart: flux.HelmReleaseChart{Spec: flux.HelmReleaseChartSpec{Chart: "from-primary"}}}},
	}
	fallback := []flux.HelmRelease{
		{Metadata: flux.ObjectMeta{Name: "podinfo", Namespace: "apps"}, Spec: flux.HelmReleaseSpec{Chart: flux.HelmReleaseChart{Spec: flux.HelmReleaseChartSpec{Chart: "from-fallback"}}}},
		{Metadata: flux.ObjectMeta{Name: "other", Namespace: "infra"}, Spec: flux.HelmReleaseSpec{Chart: flux.HelmReleaseChart{Spec: flux.HelmReleaseChartSpec{Chart: "other-chart"}}}},
	}

	result := dedupHelmReleases(primary, fallback)
	if len(result) != 2 {
		t.Fatalf("expected 2 after dedup, got %d: %+v", len(result), result)
	}

	// podinfo should come from primary.
	var podinfo *flux.HelmRelease
	for i := range result {
		if result[i].Metadata.Name == "podinfo" {
			podinfo = &result[i]
		}
	}
	if podinfo == nil {
		t.Fatal("podinfo not found in result")
	}
	if podinfo.Spec.Chart.Spec.Chart != "from-primary" {
		t.Errorf("podinfo chart = %q, want %q (primary should win)", podinfo.Spec.Chart.Spec.Chart, "from-primary")
	}
}

// Test 4b: Two Flux Kustomizations referencing the same shared base produce
// only one HelmRelease instance after dedup.
func TestDedupHelmReleases_SharedBaseReferencedTwice(t *testing.T) {
	repoRoot := t.TempDir()

	baseDir := filepath.Join(repoRoot, "apps", "base")
	writeHelper(t, baseDir, "helmrelease.yaml", `apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: podinfo
  namespace: apps
spec:
  chart:
    spec:
      chart: podinfo
`)
	writeHelper(t, baseDir, "kustomization.yaml", `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - helmrelease.yaml
`)

	// Two Flux Kustomizations pointing to the same shared base.
	clusterPath := filepath.Join(repoRoot, "clusters", "test")
	writeHelper(t, clusterPath, "base-a.yaml", `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: base-a
  namespace: flux-system
spec:
  path: ../../apps/base
  sourceRef:
    kind: GitRepository
    name: flux-system
`)
	writeHelper(t, clusterPath, "base-b.yaml", `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: base-b
  namespace: flux-system
spec:
  path: ../../apps/base
  sourceRef:
    kind: GitRepository
    name: flux-system
`)

	ctx := context.Background()
	kustomizations, err := flux.NewParser(clusterPath).ParseKustomizations(ctx)
	if err != nil {
		t.Fatalf("ParseKustomizations: %v", err)
	}

	builder := kustomize.NewBuilder()
	buildCache := make(map[string][]byte)
	output, err := buildKSContent(ctx, builder, kustomizations, repoRoot, clusterPath, nil, true, buildCache)
	if err != nil {
		t.Fatalf("buildKSContent: %v", err)
	}

	primaryHRs, _ := flux.ParseHelmReleasesFromBytes(output)
	// Without dedup, the same HR may appear twice (once per KS). Dedup must collapse to one.
	deduped := dedupHelmReleases(primaryHRs, nil)
	if len(deduped) != 1 {
		t.Errorf("expected 1 HelmRelease after dedup (same base referenced twice), got %d", len(deduped))
	}
}

// Test 5: resolveHelmInflationSources finds HelmRepository defined outside
// clusterPath but inside repoRoot via the repoRoot fallback.
func TestResolveHelmInflationSources_FallbackToRepoRoot(t *testing.T) {
	repoRoot := t.TempDir()

	// HelmRepository in a shared sources/ directory outside clusterPath.
	sourcesDir := filepath.Join(repoRoot, "sources")
	writeHelper(t, sourcesDir, "helmrepo.yaml", `apiVersion: source.toolkit.fluxcd.io/v1
kind: HelmRepository
metadata:
  name: podinfo
  namespace: apps
spec:
  url: https://stefanprodan.github.io/podinfo
`)

	// clusterPath has no HelmRepository.
	clusterPath := filepath.Join(repoRoot, "clusters", "test")
	writeHelper(t, clusterPath, "placeholder.yaml", `apiVersion: v1
kind: ConfigMap
metadata:
  name: placeholder
`)

	helmRepos, ociRepos, _, _ := resolveHelmInflationSources(context.Background(), clusterPath, repoRoot)
	if len(helmRepos) != 1 {
		t.Fatalf("expected 1 HelmRepository from repoRoot fallback, got %d", len(helmRepos))
	}
	if helmRepos[0].Metadata.Name != "podinfo" {
		t.Errorf("HelmRepository name = %q, want %q", helmRepos[0].Metadata.Name, "podinfo")
	}
	if !strings.Contains(helmRepos[0].Spec.URL, "podinfo") {
		t.Errorf("HelmRepository URL = %q, want one containing 'podinfo'", helmRepos[0].Spec.URL)
	}
	if len(ociRepos) != 0 {
		t.Errorf("expected 0 OCIRepositories, got %d", len(ociRepos))
	}
}

// Test 6: inflateHelmReleasesShared prints a warning (not silence) when a
// HelmRelease's source cannot be resolved.
func TestInflateHelmReleasesShared_WarnOnMissingSource(t *testing.T) {
	hr := []flux.HelmRelease{
		{
			Metadata: flux.ObjectMeta{Name: "podinfo", Namespace: "apps"},
			Spec: flux.HelmReleaseSpec{
				Chart: flux.HelmReleaseChart{
					Spec: flux.HelmReleaseChartSpec{
						Chart: "podinfo",
						SourceRef: struct {
							Kind      string `yaml:"kind"`
							Name      string `yaml:"name"`
							Namespace string `yaml:"namespace,omitempty"`
						}{
							Kind: "HelmRepository",
							Name: "missing-repo",
						},
					},
				},
			},
		},
	}

	inflater, err := helm.NewInflater()
	if err != nil {
		t.Fatalf("NewInflater: %v", err)
	}

	stderr := captureStderr(func() {
		outputs := inflateHelmReleasesShared(context.Background(), inflater, hr, nil, nil, nil, nil, false)
		if len(outputs) != 0 {
			t.Errorf("expected 0 outputs (source unresolved), got %d", len(outputs))
		}
	})

	if !strings.Contains(stderr, "Warning:") {
		t.Errorf("expected warning in stderr, got:\n%s", stderr)
	}
	if !strings.Contains(stderr, "podinfo") {
		t.Errorf("expected HR name 'podinfo' in warning, got:\n%s", stderr)
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
