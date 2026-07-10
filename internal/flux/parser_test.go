package flux

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseSingleDocument(t *testing.T) {
	tests := []struct {
		name     string
		yaml     string
		wantKind string
		wantName string
		wantNil  bool
	}{
		{
			name: "Kustomization resource",
			yaml: `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: apps
  namespace: flux-system
spec:
  path: ./apps
  sourceRef:
    apiVersion: source.toolkit.fluxcd.io/v1
    kind: GitRepository
    name: flux-system
`,
			wantKind: KindKustomization,
			wantName: "apps",
		},
		{
			name: "HelmRelease resource",
			yaml: `apiVersion: helm.toolkit.fluxcd.io/v2beta1
kind: HelmRelease
metadata:
  name: podinfo
  namespace: flux-system
spec:
  chart:
    spec:
      chart: podinfo
      version: 6.0.0
      sourceRef:
        kind: HelmRepository
        name: podinfo
`,
			wantKind: KindHelmRelease,
			wantName: "podinfo",
		},
		{
			name: "HelmRepository resource",
			yaml: `apiVersion: source.toolkit.fluxcd.io/v1beta2
kind: HelmRepository
metadata:
  name: podinfo
  namespace: flux-system
spec:
  url: https://stefanprodan.github.io/podinfo
`,
			wantKind: KindHelmRepository,
			wantName: "podinfo",
		},
		{
			name:    "empty document",
			yaml:    ``,
			wantNil: true,
		},
		{
			name: "non-Flux resource",
			yaml: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: test
`,
			wantNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := parseSingleDocument([]byte(tt.yaml))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantNil {
				if result != nil {
					t.Errorf("expected nil result, got %T", result)
				}
				return
			}
			if result == nil {
				t.Fatalf("expected non-nil result")
			}

			var gotName, gotKind string
			switch v := result.(type) {
			case Kustomization:
				gotName = v.Metadata.Name
				gotKind = v.Kind
			case HelmRelease:
				gotName = v.Metadata.Name
				gotKind = v.Kind
			case HelmRepository:
				gotName = v.Metadata.Name
				gotKind = v.Kind
			default:
				t.Fatalf("unexpected type %T", result)
			}

			if gotKind != tt.wantKind {
				t.Errorf("kind = %q, want %q", gotKind, tt.wantKind)
			}
			if gotName != tt.wantName {
				t.Errorf("name = %q, want %q", gotName, tt.wantName)
			}
		})
	}
}

func TestParseKustomizations(t *testing.T) {
	// Create a temp directory with test YAML files.
	tmpDir := t.TempDir()

	yamlContent := `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: apps
  namespace: flux-system
spec:
  path: ./apps
  sourceRef:
    apiVersion: source.toolkit.fluxcd.io/v1
    kind: GitRepository
    name: flux-system
`
	err := os.WriteFile(filepath.Join(tmpDir, "kustomization.yaml"), []byte(yamlContent), 0644)
	if err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	// Also write a non-flux file that should be skipped.
	nonFluxContent := `apiVersion: v1
kind: ConfigMap
metadata:
  name: test
data:
  key: value
`
	err = os.WriteFile(filepath.Join(tmpDir, "configmap.yaml"), []byte(nonFluxContent), 0644)
	if err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	parser := NewParser(tmpDir)
	results, err := parser.ParseKustomizations(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 Kustomization, got %d", len(results))
	}

	if results[0].Metadata.Name != "apps" {
		t.Errorf("name = %q, want %q", results[0].Metadata.Name, "apps")
	}
	if results[0].Spec.Path != "./apps" {
		t.Errorf("path = %q, want %q", results[0].Spec.Path, "./apps")
	}
}

func TestParseHelmReleases(t *testing.T) {
	tmpDir := t.TempDir()

	yamlContent := `apiVersion: helm.toolkit.fluxcd.io/v2beta1
kind: HelmRelease
metadata:
  name: podinfo
  namespace: flux-system
spec:
  chart:
    spec:
      chart: podinfo
      version: 6.0.0
      sourceRef:
        kind: HelmRepository
        name: podinfo
        namespace: flux-system
  values:
    replicaCount: 2
`
	err := os.WriteFile(filepath.Join(tmpDir, "helmrelease.yaml"), []byte(yamlContent), 0644)
	if err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	parser := NewParser(tmpDir)
	results, err := parser.ParseHelmReleases(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 HelmRelease, got %d", len(results))
	}

	if results[0].Metadata.Name != "podinfo" {
		t.Errorf("name = %q, want %q", results[0].Metadata.Name, "podinfo")
	}
	if results[0].Spec.Chart.Spec.Chart != "podinfo" {
		t.Errorf("chart = %q, want %q", results[0].Spec.Chart.Spec.Chart, "podinfo")
	}
	if results[0].Spec.Values["replicaCount"] != nil {
		val := results[0].Spec.Values["replicaCount"]
		if val != 2 {
			t.Errorf("replicaCount = %v, want 2", val)
		}
	}
}

func TestParseMultiDocumentYAML(t *testing.T) {
	tmpDir := t.TempDir()

	// Write a multi-document YAML file.
	multiDoc := `apiVersion: source.toolkit.fluxcd.io/v1beta2
kind: HelmRepository
metadata:
  name: podinfo
  namespace: flux-system
spec:
  url: https://stefanprodan.github.io/podinfo
---
apiVersion: helm.toolkit.fluxcd.io/v2beta1
kind: HelmRelease
metadata:
  name: podinfo
  namespace: flux-system
spec:
  chart:
    spec:
      chart: podinfo
      version: 6.0.0
      sourceRef:
        kind: HelmRepository
        name: podinfo
`
	err := os.WriteFile(filepath.Join(tmpDir, "resources.yaml"), []byte(multiDoc), 0644)
	if err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	parser := NewParser(tmpDir)

	repos, err := parser.ParseHelmRepositories(context.Background())
	if err != nil {
		t.Fatalf("parsing HelmRepositories: %v", err)
	}
	if len(repos) != 1 {
		t.Errorf("expected 1 HelmRepository, got %d", len(repos))
	}

	releases, err := parser.ParseHelmReleases(context.Background())
	if err != nil {
		t.Fatalf("parsing HelmReleases: %v", err)
	}
	if len(releases) != 1 {
		t.Errorf("expected 1 HelmRelease, got %d", len(releases))
	}
}

func TestParseKustomizations_RealWorldFormat(t *testing.T) {
	tmpDir := t.TempDir()

	// Real-world Flux Kustomization format from user's repository
	yamlContent := `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: base
  namespace: flux-system
spec:
  interval: 5m
  retryInterval: 1m
  path: ./k8s/clusters/infra/apps/base
  wait: false
  prune: true
  sourceRef:
    kind: GitRepository
    name: flux-system
  decryption:
    provider: sops
  dependsOn:
    - name: system
  postBuild:
    substituteFrom:
      - kind: ConfigMap
        name: cluster-settings
      - kind: Secret
        name: cluster-secrets
`
	err := os.WriteFile(filepath.Join(tmpDir, "base.yaml"), []byte(yamlContent), 0644)
	if err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	parser := NewParser(tmpDir)
	results, err := parser.ParseKustomizations(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 Kustomization, got %d", len(results))
	}

	ks := results[0]
	if ks.Metadata.Name != "base" {
		t.Errorf("name = %q, want %q", ks.Metadata.Name, "base")
	}
	if ks.Spec.Path != "./k8s/clusters/infra/apps/base" {
		t.Errorf("path = %q, want %q", ks.Spec.Path, "./k8s/clusters/infra/apps/base")
	}
	if ks.Spec.Wait != false {
		t.Errorf("wait = %v, want false", ks.Spec.Wait)
	}
	if ks.Spec.Prune != true {
		t.Errorf("prune = %v, want true", ks.Spec.Prune)
	}
	if len(ks.Spec.DependsOn) != 1 || ks.Spec.DependsOn[0].Name != "system" {
		t.Errorf("dependsOn = %v, want [{name: system}]", ks.Spec.DependsOn)
	}
	if ks.Spec.SourceRef.Kind != "GitRepository" {
		t.Errorf("sourceRef.kind = %q, want %q", ks.Spec.SourceRef.Kind, "GitRepository")
	}
}

func TestParseKustomizations_EmptyDir(t *testing.T) {
	tmpDir := t.TempDir()

	parser := NewParser(tmpDir)
	_, err := parser.ParseKustomizations(context.Background())
	if err == nil {
		t.Fatal("expected error for empty directory, got nil")
	}
}

func TestIsYAMLFile(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"test.yaml", true},
		{"test.yml", true},
		{"test.YAML", true},
		{"test.YML", true},
		{"test.json", false},
		{"test.txt", false},
		{"yaml", false},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := isYAMLFile(tt.path)
			if got != tt.want {
				t.Errorf("isYAMLFile(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestParsePartialHelmRelease(t *testing.T) {
	tmpDir := t.TempDir()

	// Partial HelmRelease: cluster-specific overlay with only values, no chart spec.
	yamlContent := `apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: infra-ingress-nginx
  namespace: flux-system
spec:
  values:
    controller:
      config:
        proxy-buffer-size: 32k
`
	err := os.WriteFile(filepath.Join(tmpDir, "overlay.yaml"), []byte(yamlContent), 0644)
	if err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	parser := NewParser(tmpDir)
	results, err := parser.ParseHelmReleases(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 HelmRelease, got %d", len(results))
	}

	hr := results[0]
	if hr.Metadata.Name != "infra-ingress-nginx" {
		t.Errorf("name = %q, want %q", hr.Metadata.Name, "infra-ingress-nginx")
	}
	// Chart spec should be empty (partial overlay).
	if hr.Spec.Chart.Spec.Chart != "" {
		t.Errorf("expected empty chart name for partial HelmRelease, got %q", hr.Spec.Chart.Spec.Chart)
	}
}

func TestIsKustomizeAPI(t *testing.T) {
	tests := []struct {
		apiVersion string
		want       bool
	}{
		{"kustomize.toolkit.fluxcd.io/v1", true},
		{"kustomize.toolkit.fluxcd.io/v1beta1", true},
		{"helm.toolkit.fluxcd.io/v2beta1", false},
		{"source.toolkit.fluxcd.io/v1", false},
	}

	for _, tt := range tests {
		t.Run(tt.apiVersion, func(t *testing.T) {
			got := isKustomizeAPI(tt.apiVersion)
			if got != tt.want {
				t.Errorf("isKustomizeAPI(%q) = %v, want %v", tt.apiVersion, got, tt.want)
			}
		})
	}
}

// TestSplitYAMLText_BlockScalarWithSeparator verifies that a literal "---"
// inside a block scalar (| or >) is NOT treated as a document separator.
// This is a regression test for the text-based splitter that would incorrectly
// split secrets, CRDs, or any YAML containing frontmatter-like content inside
// block scalars — potentially leaving secret values unredacted.
func TestSplitYAMLText_BlockScalarWithSeparator(t *testing.T) {
	input := []byte(`apiVersion: v1
kind: Secret
metadata:
  name: cert
data:
  tls.crt: |
    -----BEGIN CERTIFICATE-----
    --- not a separator
    MIIB...
    -----END CERTIFICATE-----
  tls.key: |
    --- also not a separator
    key-data
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: config
data:
  key: value
`)

	docs := SplitYAMLText(input)

	if len(docs) != 2 {
		t.Fatalf("expected 2 documents, got %d — block scalar --- was treated as separator", len(docs))
	}

	// First document should be the Secret with its full content.
	if !strings.Contains(docs[0], "BEGIN CERTIFICATE") {
		t.Error("Secret document missing certificate content")
	}
	if !strings.Contains(docs[0], "not a separator") {
		t.Error("Secret document missing block scalar content with ---")
	}
	if !strings.Contains(docs[0], "also not a separator") {
		t.Error("Secret document missing tls.key block scalar content")
	}

	// Second document should be the ConfigMap.
	if !strings.Contains(docs[1], "ConfigMap") {
		t.Error("second document should be ConfigMap")
	}
}

// TestSplitYAMLText_BlockScalarWithSeparatorRedaction verifies that
// RedactSecrets correctly redacts secret values when the secret data
// contains "---" inside a block scalar.
func TestSplitYAMLText_BlockScalarWithSeparatorRedaction(t *testing.T) {
	input := []byte(`apiVersion: v1
kind: Secret
metadata:
  name: cert
data:
  tls.crt: |
    -----BEGIN CERTIFICATE-----
    --- not a separator
    MIIB...
`)

	redacted := RedactSecrets(input)

	if strings.Contains(string(redacted), "MIIB") {
		t.Error("secret value leaked — RedactSecrets did not redact block scalar content")
	}
	if !strings.Contains(string(redacted), SecretRedactedValue) {
		t.Error("expected redacted placeholder in output")
	}
}
