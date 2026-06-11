package flux

import (
	"context"
	"os"
	"path/filepath"
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
			name: "GitRepository resource",
			yaml: `apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: flux-system
  namespace: flux-system
spec:
  url: https://github.com/example/repo.git
  ref:
    branch: main
`,
			wantKind: KindGitRepository,
			wantName: "flux-system",
		},
		{
			name:    "empty document",
			yaml:    ``,
			wantNil: true,
		},
		{
			name: "non-Flux resource",
			yaml: `apiVersion: v1
kind: ConfigMap
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
			case GitRepository:
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
