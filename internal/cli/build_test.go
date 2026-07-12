package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// TestReorderYAMLFields_BlockScalarWithSeparator verifies that a literal "---"
// inside a block scalar (|) does NOT split the document during reordering.
func TestReorderYAMLFields_BlockScalarWithSeparator(t *testing.T) {
	input := []byte(`kind: Secret
metadata:
  name: cert
apiVersion: v1
data:
  tls.crt: |
    -----BEGIN CERTIFICATE-----
    --- not a separator
    MIIB...
    -----END CERTIFICATE-----
---
kind: ConfigMap
metadata:
  name: config
apiVersion: v1
`)
	result := reorderYAMLFields(input)
	resultStr := string(result)

	// First document must be intact — certificate content must NOT be split off.
	if !strings.Contains(resultStr, "BEGIN CERTIFICATE") {
		t.Error("certificate content lost — block scalar --- was treated as separator")
	}
	if !strings.Contains(resultStr, "not a separator") {
		t.Error("block scalar line with --- lost during reorder")
	}

	// Second document must still be present after the real separator.
	if !strings.Contains(resultStr, "ConfigMap") {
		t.Error("second document (ConfigMap) missing after real separator")
	}

	// Must have exactly 2 documents (1 real separator).
	sepCount := strings.Count(resultStr, "\n---\n")
	if sepCount != 1 {
		t.Errorf("expected 1 document separator in output, got %d", sepCount)
	}
}

func TestProcessYAMLDoc_StripsSOPS(t *testing.T) {
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
	result := processYAMLDoc(input)
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

func TestProcessYAMLDoc_NoSOPS(t *testing.T) {
	input := []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: test
data:
  key: value
`)
	result := processYAMLDoc(input)
	resultStr := string(result)

	// Substring checks (not exact equality) — yaml.Node round-trip
	// may change formatting details like quoting or spacing.
	if indexOf(resultStr, "ConfigMap") < 0 {
		t.Error("expected kind to be preserved")
	}
	if indexOf(resultStr, "key: value") < 0 {
		t.Error("expected data.key to be preserved")
	}
	if indexOf(resultStr, "sops:") >= 0 {
		t.Error("did not expect sops section in output")
	}
}

func TestProcessYAMLDoc_ListItem(t *testing.T) {
	// A YAML list at top level should be preserved (no reorder applied).
	input := []byte("- name: item1\n- name: item2\n")
	result := processYAMLDoc(input)
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

// captureStdout temporarily redirects os.Stdout, runs f, and returns what was written.
func captureStdout(f func()) string {
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	f()
	w.Close()
	os.Stdout = old
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

	builder := kustomize.NewBuilder(repoRoot)
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

// Test: Two Flux Kustomizations referencing the same shared base — the
// HelmRelease appears in the build output (possibly twice), dedup is handled
// internally by buildHRInflation.
func TestBuildKSContent_SharedBaseReferencedTwice(t *testing.T) {
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

	builder := kustomize.NewBuilder(repoRoot)
	buildCache := make(map[string][]byte)
	output, err := buildKSContent(ctx, builder, kustomizations, repoRoot, clusterPath, nil, true, buildCache)
	if err != nil {
		t.Fatalf("buildKSContent: %v", err)
	}

	// The HR should be present in the build output.
	helmReleases, _ := flux.ParseHelmReleasesFromBytes(output)
	if len(helmReleases) == 0 {
		t.Fatal("expected at least 1 HelmRelease from shared base, got 0")
	}

	// Dedup by namespace/name (same logic as buildHRInflation).
	seen := make(map[string]bool)
	for _, hr := range helmReleases {
		key := hr.Metadata.Namespace + "/" + hr.Metadata.Name
		seen[key] = true
	}
	if len(seen) != 1 {
		t.Errorf("expected 1 unique HelmRelease after dedup, got %d: %v", len(seen), seen)
	}
}

func TestConvertJSONInYAMLToYAML_MultiDoc(t *testing.T) {
	input := []byte(`apiVersion: v1
kind: ServiceAccount
metadata:
  name: sa1
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: sa2
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: cm1
`)
	result, err := helm.ConvertJSONInYAMLToYAML(input)
	if err != nil {
		t.Fatalf("helm.ConvertJSONInYAMLToYAML: %v", err)
	}
	resultStr := string(result)

	// All three documents must be present.
	if !strings.Contains(resultStr, "sa1") {
		t.Error("first document (sa1) was dropped")
	}
	if !strings.Contains(resultStr, "sa2") {
		t.Error("second document (sa2) was dropped")
	}
	if !strings.Contains(resultStr, "cm1") {
		t.Error("third document (cm1) was dropped")
	}

	// Must have document separators.
	if strings.Count(resultStr, "\n---\n") < 2 {
		t.Errorf("expected at least 2 document separators, got %d in:\n%s", strings.Count(resultStr, "\n---\n"), resultStr)
	}
}

// Test: helm.ConvertJSONInYAMLToYAML returns nil for empty/nil input (no "null" output).
func TestConvertJSONInYAMLToYAML_Empty(t *testing.T) {
	result, err := helm.ConvertJSONInYAMLToYAML(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil for empty input, got %q", string(result))
	}
}

// Test: helm.ConvertJSONInYAMLToYAML terminates (doesn't infinite-loop) on
// malformed YAML input, and preserves subsequent valid documents.
func TestConvertJSONInYAMLToYAML_MalformedTerminates(t *testing.T) {
	// Broken document followed by a valid one.
	input := []byte(`::: broken yaml :::
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: survives
`)

	done := make(chan struct{})
	var result []byte
	go func() {
		result, _ = helm.ConvertJSONInYAMLToYAML(input)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ConvertJSONInYAMLToYAML hung on malformed input — infinite loop")
	}

	// The valid document after the broken one must be preserved.
	resultStr := string(result)
	if !strings.Contains(resultStr, "survives") {
		t.Errorf("valid document after broken one was dropped:\n%s", resultStr)
	}
}

// Test: helm.ConvertJSONInYAMLToYAML strips nil values (annotations: null, labels: null, etc.)
// produced by Helm templates with empty optional fields.
func TestConvertJSONInYAMLToYAML_StripsNilValues(t *testing.T) {
	input := []byte(`apiVersion: v1
kind: ServiceAccount
metadata:
  annotations: null
  labels:
    app: test
  name: sa1
  namespace: default
spec: null
`)
	result, err := helm.ConvertJSONInYAMLToYAML(input)
	if err != nil {
		t.Fatalf("helm.ConvertJSONInYAMLToYAML: %v", err)
	}
	resultStr := string(result)

	// Nil values must be removed.
	if strings.Contains(resultStr, "annotations: null") {
		t.Errorf("expected 'annotations: null' to be stripped:\n%s", resultStr)
	}
	if strings.Contains(resultStr, "spec: null") {
		t.Errorf("expected 'spec: null' to be stripped:\n%s", resultStr)
	}

	// Non-nil values must be preserved.
	if !strings.Contains(resultStr, "labels:") {
		t.Errorf("expected 'labels' to be preserved:\n%s", resultStr)
	}
	if !strings.Contains(resultStr, "app: test") {
		t.Errorf("expected 'app: test' to be preserved:\n%s", resultStr)
	}
}

// Test: helm.RemoveNilValues recursively removes nil map entries at any nesting depth.
func TestRemoveNilValues(t *testing.T) {
	input := map[string]interface{}{
		"keep": "value",
		"drop": nil,
		"nested": map[string]interface{}{
			"keep2": 42,
			"drop2": nil,
			"deep": map[string]interface{}{
				"keep3": "deep",
				"drop3": nil,
			},
		},
		"list": []interface{}{"a", nil, "b"},
	}

	result := helm.RemoveNilValues(input)
	m := result.(map[string]interface{})

	if _, exists := m["drop"]; exists {
		t.Error("expected top-level nil key to be removed")
	}
	if m["keep"] != "value" {
		t.Error("expected non-nil value to be preserved")
	}

	nested := m["nested"].(map[string]interface{})
	if _, exists := nested["drop2"]; exists {
		t.Error("expected nested nil key to be removed")
	}
	if nested["keep2"] != 42 {
		t.Error("expected nested non-nil value to be preserved")
	}

	deep := nested["deep"].(map[string]interface{})
	if _, exists := deep["drop3"]; exists {
		t.Error("expected deep nil key to be removed")
	}
	if deep["keep3"] != "deep" {
		t.Error("expected deep non-nil value to be preserved")
	}
}

// Test: hasDirectKustomizations — build hr requires Flux Kustomization files
// directly in the path (same contract as build ks).
func TestHasDirectKustomizations(t *testing.T) {
	// Path with a Flux Kustomization file.
	withKS := t.TempDir()
	writeHelper(t, withKS, "base.yaml", `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: base
  namespace: flux-system
spec:
  path: ./base
`)
	has, err := hasDirectKustomizations(withKS)
	if err != nil {
		t.Fatalf("hasDirectKustomizations: %v", err)
	}
	if !has {
		t.Error("expected true for directory with Flux Kustomization file")
	}

	// Path with only native kustomize (no Flux Kustomization).
	nativeOnly := t.TempDir()
	writeHelper(t, nativeOnly, "kustomization.yaml", `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources: []
`)
	has, err = hasDirectKustomizations(nativeOnly)
	if err != nil {
		t.Fatalf("hasDirectKustomizations: %v", err)
	}
	if has {
		t.Error("expected false for directory with only native kustomization")
	}

	// Empty directory.
	empty := t.TempDir()
	has, err = hasDirectKustomizations(empty)
	if err != nil {
		t.Fatalf("hasDirectKustomizations: %v", err)
	}
	if has {
		t.Error("expected false for empty directory")
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
		outputs := inflateHelmReleasesShared(context.Background(), inflater, hr, nil, nil, nil, nil, false, false)
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

// Test: printResourcesBoxed outputs each resource with a box header and
// sorts by kind/namespace/name.
func TestPrintResourcesBoxed(t *testing.T) {
	input := []byte(`apiVersion: v1
kind: Service
metadata:
  name: svc2
  namespace: z-ns
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: cm1
  namespace: a-ns
---
apiVersion: v1
kind: Service
metadata:
  name: svc1
  namespace: a-ns
`)

	output := captureStdout(func() {
		printResourcesBoxed(input)
	})

	// All three resources must have box headers.
	if !strings.Contains(output, "ConfigMap: a-ns/cm1") {
		t.Error("missing ConfigMap box header")
	}
	if !strings.Contains(output, "Service: a-ns/svc1") {
		t.Error("missing Service svc1 box header")
	}
	if !strings.Contains(output, "Service: z-ns/svc2") {
		t.Error("missing Service svc2 box header")
	}

	// ConfigMap must come before Service (sorted by kind).
	cmIdx := strings.Index(output, "ConfigMap:")
	svcIdx := strings.Index(output, "Service:")
	if cmIdx < 0 || svcIdx < 0 || cmIdx > svcIdx {
		t.Errorf("expected ConfigMap before Service (sorted by kind):\n%s", output)
	}

	// svc1 (a-ns) must come before svc2 (z-ns) (sorted by namespace within kind).
	svc1Idx := strings.Index(output, "svc1")
	svc2Idx := strings.Index(output, "svc2")
	if svc1Idx < 0 || svc2Idx < 0 || svc1Idx > svc2Idx {
		t.Errorf("expected svc1 (a-ns) before svc2 (z-ns):\n%s", output)
	}

	// Each resource must have border lines.
	if strings.Count(output, "---") < 6 {
		t.Errorf("expected at least 6 border lines (2 per resource), got fewer:\n%s", output)
	}
}

// Test: printResourcesBoxed produces no output for nil/empty input.
func TestPrintResourcesBoxed_Empty(t *testing.T) {
	output := captureStdout(func() {
		printResourcesBoxed(nil)
	})
	if output != "" {
		t.Errorf("expected empty output for nil input, got %q", output)
	}

	output = captureStdout(func() {
		printResourcesBoxed([]byte(""))
	})
	if output != "" {
		t.Errorf("expected empty output for empty input, got %q", output)
	}
}

// TestBuildSourcePath_SubdirectoryKustomization verifies that when
// buildSourcePath encounters a directory without a top-level kustomization.yaml
// but with subdirectories that have their own kustomization.yaml (with a
// namespace transformer), the namespace is correctly applied to the output.
func TestBuildSourcePath_SubdirectoryKustomization(t *testing.T) {
	repoRoot := t.TempDir()

	// Source path: directory WITHOUT kustomization.yaml at top level.
	sourcePath := filepath.Join(repoRoot, "apps")

	// Subdirectory with kustomization.yaml that sets namespace.
	appDir := filepath.Join(sourcePath, "podinfo")
	writeHelper(t, appDir, "kustomization.yaml", `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: podinfo
resources:
  - helmrelease.yaml
  - ocirepository.yaml
`)
	writeHelper(t, appDir, "helmrelease.yaml", `apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: podinfo
spec:
  interval: 5m
  chartRef:
    kind: OCIRepository
    name: podinfo
`)
	writeHelper(t, appDir, "ocirepository.yaml", `apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: podinfo
spec:
  interval: 6h
  url: oci://ghcr.io/stefanprodan/charts/podinfo
`)

	builder := kustomize.NewBuilder(repoRoot)
	output, err := buildSourcePath(builder, sourcePath)
	if err != nil {
		t.Fatalf("buildSourcePath: %v", err)
	}

	outputStr := string(output)

	// Namespace from kustomization.yaml must be applied to HelmRelease.
	if !strings.Contains(outputStr, "kind: HelmRelease") {
		t.Error("HelmRelease missing from output")
	}
	if !strings.Contains(outputStr, "namespace: podinfo") {
		t.Errorf("namespace: podinfo not applied by kustomize transformer:\n%s", outputStr)
	}

	// OCIRepository should also have the namespace.
	if !strings.Contains(outputStr, "kind: OCIRepository") {
		t.Error("OCIRepository missing from output")
	}
}

// TestBuildSourcePath_LooseYAML verifies that loose YAML files (not inside
// any kustomization.yaml) are still read when mixed with kustomized dirs.
func TestBuildSourcePath_LooseYAML(t *testing.T) {
	repoRoot := t.TempDir()
	sourcePath := filepath.Join(repoRoot, "mixed")

	// Subdirectory with kustomization.yaml.
	overlayDir := filepath.Join(sourcePath, "overlay")
	writeHelper(t, overlayDir, "kustomization.yaml", `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: test-ns
resources:
  - cm.yaml
`)
	writeHelper(t, overlayDir, "cm.yaml", `apiVersion: v1
kind: ConfigMap
metadata:
  name: from-kustomize
`)

	// Loose YAML file at the source path level.
	writeHelper(t, sourcePath, "loose.yaml", `apiVersion: v1
kind: ConfigMap
metadata:
  name: loose-file
`)

	builder := kustomize.NewBuilder(repoRoot)
	output, err := buildSourcePath(builder, sourcePath)
	if err != nil {
		t.Fatalf("buildSourcePath: %v", err)
	}

	outputStr := string(output)

	// Kustomized resource should have namespace applied.
	if !strings.Contains(outputStr, "from-kustomize") {
		t.Error("resource from kustomized subdirectory missing")
	}
	if !strings.Contains(outputStr, "namespace: test-ns") {
		t.Error("namespace transformer not applied to kustomized resource")
	}

	// Loose file should also be present (without namespace).
	if !strings.Contains(outputStr, "loose-file") {
		t.Error("loose YAML file missing from output")
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
