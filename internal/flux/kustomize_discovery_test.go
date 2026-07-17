package flux

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverKustomizeDirs(t *testing.T) {
	tmpDir := t.TempDir()

	// Create structure matching user's real repo:
	// k8s/clusters/infra/flux/
	// ├── base.yaml          (Flux Kustomization — should NOT be discovered)
	// ├── vars/
	// │   ├── kustomization.yaml   (native kustomize — SHOULD be discovered)
	// │   └── cluster-settings.yaml
	// ├── nested/
	// │   └── subdir/
	// │       ├── kustomization.yaml   (native kustomize — SHOULD be discovered)
	// │       └── data.yaml

	fluxDir := filepath.Join(tmpDir, "flux")
	varsDir := filepath.Join(fluxDir, "vars")
	nestedDir := filepath.Join(fluxDir, "nested", "subdir")

	if err := os.MkdirAll(varsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(nestedDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Flux Kustomization (should NOT be discovered)
	fluxKS := `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: base
  namespace: flux-system
spec:
  path: ./apps/base
  sourceRef:
    kind: GitRepository
    name: flux-system
`
	if err := os.WriteFile(filepath.Join(fluxDir, "base.yaml"), []byte(fluxKS), 0644); err != nil {
		t.Fatal(err)
	}

	// Native kustomize in vars/ (SHOULD be discovered)
	varsKust := `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../../../../vars
patches:
  - path: cluster-settings.yaml
`
	if err := os.WriteFile(filepath.Join(varsDir, "kustomization.yaml"), []byte(varsKust), 0644); err != nil {
		t.Fatal(err)
	}

	// Native kustomize in nested/subdir/ (SHOULD be discovered)
	nestedKust := `kind: Kustomization
resources:
  - data.yaml
`
	if err := os.WriteFile(filepath.Join(nestedDir, "kustomization.yaml"), []byte(nestedKust), 0644); err != nil {
		t.Fatal(err)
	}

	// Run discovery
	dirs, err := DiscoverKustomizeDirs(context.Background(), fluxDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(dirs) != 2 {
		t.Fatalf("expected 2 kustomize dirs, got %d: %v", len(dirs), dirs)
	}

	// Check that vars/ was found
	foundVars := false
	foundNested := false
	for _, dir := range dirs {
		if filepath.Base(dir) == "vars" {
			foundVars = true
		}
		if filepath.Base(dir) == "subdir" {
			foundNested = true
		}
	}
	if !foundVars {
		t.Error("vars/ directory was not discovered")
	}
	if !foundNested {
		t.Error("nested/subdir/ directory was not discovered")
	}
}

func TestDiscoverKustomizeDirs_SkipsRoot(t *testing.T) {
	tmpDir := t.TempDir()

	// Root dir has kustomization.yaml — should be skipped
	rootKust := `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - app.yaml
`
	if err := os.WriteFile(filepath.Join(tmpDir, "kustomization.yaml"), []byte(rootKust), 0644); err != nil {
		t.Fatal(err)
	}

	// Subdir also has kustomization.yaml — should be found
	subDir := filepath.Join(tmpDir, "subdir")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "kustomization.yaml"), []byte(rootKust), 0644); err != nil {
		t.Fatal(err)
	}

	dirs, err := DiscoverKustomizeDirs(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(dirs) != 1 {
		t.Fatalf("expected 1 dir (subdir only), got %d: %v", len(dirs), dirs)
	}
	if filepath.Base(dirs[0]) != "subdir" {
		t.Errorf("expected subdir, got %s", dirs[0])
	}
}

func TestDiscoverKustomizeDirs_SkipsFluxKustomization(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a dir with Flux Kustomization in kustomization.yaml
	fluxDir := filepath.Join(tmpDir, "flux-ks")
	if err := os.MkdirAll(fluxDir, 0755); err != nil {
		t.Fatal(err)
	}

	fluxKust := `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: apps
`
	if err := os.WriteFile(filepath.Join(fluxDir, "kustomization.yaml"), []byte(fluxKust), 0644); err != nil {
		t.Fatal(err)
	}

	dirs, err := DiscoverKustomizeDirs(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(dirs) != 0 {
		t.Errorf("expected 0 dirs (Flux KS should be skipped), got %d: %v", len(dirs), dirs)
	}
}

func TestDiscoverKustomizeDirs_NoKustomizeDirs(t *testing.T) {
	tmpDir := t.TempDir()

	// Only YAML files, no kustomization.yaml anywhere
	if err := os.WriteFile(filepath.Join(tmpDir, "app.yaml"), []byte("key: value"), 0644); err != nil {
		t.Fatal(err)
	}

	dirs, err := DiscoverKustomizeDirs(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(dirs) != 0 {
		t.Errorf("expected 0 dirs, got %d", len(dirs))
	}
}

func TestParseConfigMapsFromBytes(t *testing.T) {
	yaml := `apiVersion: v1
kind: ConfigMap
metadata:
  name: cluster-settings
  namespace: flux-system
data:
  CLUSTER_NAME: prod-us-east
  DOMAIN: example.com
---
apiVersion: v1
kind: Secret
metadata:
  name: my-secret
type: Opaque
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: other-settings
  namespace: default
data:
  FOO: bar
`
	cms, err := ParseConfigMapsFromBytes([]byte(yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(cms) != 2 {
		t.Fatalf("expected 2 ConfigMaps, got %d", len(cms))
	}

	if cms[0].Metadata.Name != "cluster-settings" {
		t.Errorf("cm[0] name = %q, want %q", cms[0].Metadata.Name, "cluster-settings")
	}
	if cms[0].Data["CLUSTER_NAME"] != "prod-us-east" {
		t.Errorf("cm[0] CLUSTER_NAME = %q, want %q", cms[0].Data["CLUSTER_NAME"], "prod-us-east")
	}
	if cms[1].Metadata.Name != "other-settings" {
		t.Errorf("cm[1] name = %q, want %q", cms[1].Metadata.Name, "other-settings")
	}
}

func TestParseConfigMapsFromBytes_Empty(t *testing.T) {
	cms, err := ParseConfigMapsFromBytes([]byte(""))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cms) != 0 {
		t.Errorf("expected 0 ConfigMaps, got %d", len(cms))
	}
}

func TestParseConfigMapsFromBytes_NoConfigMaps(t *testing.T) {
	yaml := `apiVersion: v1
kind: Secret
metadata:
  name: my-secret
type: Opaque
`
	cms, err := ParseConfigMapsFromBytes([]byte(yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cms) != 0 {
		t.Errorf("expected 0 ConfigMaps, got %d", len(cms))
	}
}

func TestIsNativeKustomize(t *testing.T) {
	tests := []struct {
		name       string
		apiVersion string
		kind       string
		want       bool
	}{
		{"native kustomize v1beta1", "kustomize.config.k8s.io/v1beta1", "Kustomization", true},
		{"native kustomize no apiVersion", "", "Kustomization", true},
		{"Flux Kustomization", "kustomize.toolkit.fluxcd.io/v1", "Kustomization", false},
		{"wrong kind", "kustomize.config.k8s.io/v1beta1", "Deployment", false},
		{"empty kind", "", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isNativeKustomize(nativeKustomization{APIVersion: tt.apiVersion, Kind: tt.kind})
			if got != tt.want {
				t.Errorf("isNativeKustomize(%+v) = %v, want %v", tt, got, tt.want)
			}
		})
	}
}

func TestEndToEnd_ConfigMapResolution(t *testing.T) {
	// Simulate the full flow: Flux KS references ConfigMap via substituteFrom,
	// ConfigMap comes from native kustomize overlay.
	tmpDir := t.TempDir()

	// Create vars/ with native kustomize overlay producing a ConfigMap
	varsDir := filepath.Join(tmpDir, "vars")
	if err := os.MkdirAll(varsDir, 0755); err != nil {
		t.Fatal(err)
	}

	varsKust := `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
generators:
  - cluster-settings.yaml
`
	if err := os.WriteFile(filepath.Join(varsDir, "kustomization.yaml"), []byte(varsKust), 0644); err != nil {
		t.Fatal(err)
	}

	// The actual ConfigMap (simulating what kustomize build would produce)
	cmYAML := `apiVersion: v1
kind: ConfigMap
metadata:
  name: cluster-settings
  namespace: flux-system
data:
  CLUSTER_NAME: prod-us-east
  DOMAIN: example.com
`
	if err := os.WriteFile(filepath.Join(varsDir, "cluster-settings.yaml"), []byte(cmYAML), 0644); err != nil {
		t.Fatal(err)
	}

	// Parse ConfigMaps directly (simulating what ParseConfigMapsFromBytes would do
	// after kustomize build)
	cms, err := ParseConfigMapsFromBytes([]byte(cmYAML))
	if err != nil {
		t.Fatalf("ParseConfigMapsFromBytes error: %v", err)
	}
	if len(cms) != 1 {
		t.Fatalf("expected 1 ConfigMap, got %d", len(cms))
	}

	// Create a Flux Kustomization that references this ConfigMap
	ks := Kustomization{
		Metadata: struct {
			Name      string `yaml:"name"`
			Namespace string `yaml:"namespace"`
		}{Name: "base", Namespace: "flux-system"},
		Spec: KustomizationSpec{
			PostBuild: &PostBuild{
				SubstituteFrom: []interface{}{
					map[string]interface{}{
						"kind": "ConfigMap",
						"name": "cluster-settings",
					},
				},
			},
		},
	}

	// Resolve substitution variables
	vars := ResolveSubstituteVars(ks, cms)
	if len(vars) != 2 {
		t.Fatalf("expected 2 vars, got %d", len(vars))
	}
	if vars["CLUSTER_NAME"] != "prod-us-east" {
		t.Errorf("CLUSTER_NAME = %q, want %q", vars["CLUSTER_NAME"], "prod-us-east")
	}
	if vars["DOMAIN"] != "example.com" {
		t.Errorf("DOMAIN = %q, want %q", vars["DOMAIN"], "example.com")
	}

	// Apply substitution to YAML output
	input := []byte("host: ${CLUSTER_NAME}.${DOMAIN}")
	result := string(ApplySubstitution(input, vars))
	expected := "host: prod-us-east.example.com"
	if result != expected {
		t.Errorf("substitution result = %q, want %q", result, expected)
	}
}

// writeFile writes content to path, creating parent directories.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}

// TestDiscoverKustomizeDirsAndFiles directly exercises the combined discovery:
// the fileDirs return (any kustomization-file kind, including orphan
// kind: Component that is NOT in buildDirs) and the buildDirs return (native
// overlays only), plus the error contract (both nil on walk error).
func TestDiscoverKustomizeDirsAndFiles(t *testing.T) {
	t.Run("fileDirs covers any kind; buildDirs only native overlays", func(t *testing.T) {
		root := t.TempDir()

		// Native kustomize overlay — in BOTH buildDirs and fileDirs.
		overlayDir := filepath.Join(root, "overlay")
		writeFile(t, filepath.Join(overlayDir, "kustomization.yaml"), `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - cm.yaml
`)
		writeFile(t, filepath.Join(overlayDir, "cm.yaml"), `apiVersion: v1
kind: ConfigMap
metadata:
  name: overlay-cm
`)

		// Orphan kind: Component dir — in fileDirs only, NOT in buildDirs.
		compDir := filepath.Join(root, "components", "foo")
		writeFile(t, filepath.Join(compDir, "kustomization.yaml"), `apiVersion: kustomize.config.k8s.io/v1alpha1
kind: Component
resources:
  - dep.yaml
`)
		writeFile(t, filepath.Join(compDir, "dep.yaml"), `apiVersion: apps/v1
kind: Deployment
metadata:
  name: comp-app
`)

		// Plain directory with no kustomization file — in neither.
		plainDir := filepath.Join(root, "plain")
		writeFile(t, filepath.Join(plainDir, "cm.yaml"), `apiVersion: v1
kind: ConfigMap
metadata:
  name: plain-cm
`)

		buildDirs, fileDirs, err := DiscoverKustomizeDirsAndFiles(context.Background(), root)
		if err != nil {
			t.Fatalf("DiscoverKustomizeDirsAndFiles: %v", err)
		}

		assertDir := func(set []string, dir, label string) {
			t.Helper()
			for _, d := range set {
				if d == dir {
					return
				}
			}
			t.Errorf("expected %q in %s, got %v", dir, label, set)
		}
		assertNotDir := func(set []string, dir, label string) {
			t.Helper()
			for _, d := range set {
				if d == dir {
					t.Errorf("did not expect %q in %s, got %v", dir, label, set)
					return
				}
			}
		}

		// buildDirs: native overlay only (Component is not a buildable overlay).
		assertDir(buildDirs, overlayDir, "buildDirs")
		assertNotDir(buildDirs, compDir, "buildDirs")
		assertNotDir(buildDirs, plainDir, "buildDirs")

		// fileDirs: overlay + Component (any kustomization file kind), not plain.
		assertDir(fileDirs, overlayDir, "fileDirs")
		assertDir(fileDirs, compDir, "fileDirs")
		assertNotDir(fileDirs, plainDir, "fileDirs")
	})

	t.Run("walk error returns nil sets", func(t *testing.T) {
		// Non-existent root: WalkDir fails on the initial Lstat.
		buildDirs, fileDirs, err := DiscoverKustomizeDirsAndFiles(
			context.Background(), filepath.Join(t.TempDir(), "does-not-exist"))
		if err == nil {
			t.Fatal("expected error for non-existent root, got nil")
		}
		if buildDirs != nil {
			t.Errorf("expected nil buildDirs on error, got %v", buildDirs)
		}
		if fileDirs != nil {
			t.Errorf("expected nil fileDirs on error, got %v", fileDirs)
		}
	})

	t.Run("honors cancelled context", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, filepath.Join(root, "overlay", "kustomization.yaml"), `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
`)

		ctx, cancel := context.WithCancel(context.Background())
		cancel() // already cancelled before the walk starts

		buildDirs, fileDirs, err := DiscoverKustomizeDirsAndFiles(ctx, root)
		// A cancelled context aborts the walk at the first directory and
		// propagates ctx.Err(); neither set is populated.
		if err == nil {
			t.Fatal("expected cancellation error, got nil")
		}
		if buildDirs != nil {
			t.Errorf("expected nil buildDirs on cancelled context, got %v", buildDirs)
		}
		if fileDirs != nil {
			t.Errorf("expected nil fileDirs on cancelled context, got %v", fileDirs)
		}
	})
}
