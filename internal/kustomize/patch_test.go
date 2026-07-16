package kustomize

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/banschikovde/fluxview/internal/yamlutil"
	"gopkg.in/yaml.v3"
)

func TestApplyPatches_JSON6902(t *testing.T) {
	resources := []byte(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: podinfo
  namespace: default
spec:
  replicas: 1
`)
	patches := []PatchSpec{
		{
			Target: &PatchTarget{
				Kind: "Deployment",
				Name: "podinfo",
			},
			Patch: `- op: replace
  path: /spec/replicas
  value: 3
`,
		},
	}

	result, err := ApplyPatches(resources, patches, "/")
	if err != nil {
		t.Fatalf("ApplyPatches: %v", err)
	}

	if !strings.Contains(string(result), "replicas: 3") {
		t.Errorf("expected replicas: 3 in output:\n%s", string(result))
	}
	if strings.Contains(string(result), "replicas: 1") {
		t.Error("expected old replicas: 1 to be replaced")
	}
}

func TestApplyPatches_NoMatch(t *testing.T) {
	resources := []byte(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: podinfo
  namespace: default
spec:
  replicas: 1
`)
	patches := []PatchSpec{
		{
			Target: &PatchTarget{
				Kind: "Service",
				Name: "nonexistent",
			},
			Patch: `- op: replace
  path: /spec/ports/0/port
  value: 8080
`,
		},
	}

	result, err := ApplyPatches(resources, patches, "/")
	if err != nil {
		t.Fatalf("ApplyPatches with no-match target should not error: %v", err)
	}
	// Original resource should be unchanged.
	if !strings.Contains(string(result), "replicas: 1") {
		t.Error("resource should be unchanged when patch target doesn't match")
	}
}

func TestApplyPatches_EmptyPatches(t *testing.T) {
	resources := []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: test
`)
	result, err := ApplyPatches(resources, nil, "/")
	if err != nil {
		t.Fatalf("ApplyPatches with nil patches: %v", err)
	}
	if string(result) != string(resources) {
		t.Error("empty patches should return resources unchanged")
	}
}

func TestApplyPatches_DuplicateResourceIDs(t *testing.T) {
	// Two resources with same kind/name/namespace — kustomize would reject
	// these in a single file, but as separate files they're fine.
	resources := []byte(`apiVersion: v1
kind: Namespace
metadata:
  name: podinfo
---
apiVersion: v1
kind: Namespace
metadata:
  name: podinfo
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: podinfo
  namespace: podinfo
spec:
  replicas: 1
`)
	patches := []PatchSpec{
		{
			Target: &PatchTarget{Kind: "Deployment", Name: "podinfo"},
			Patch: `- op: replace
  path: /spec/replicas
  value: 3
`,
		},
	}

	result, err := ApplyPatches(resources, patches, "/")
	if err != nil {
		t.Fatalf("ApplyPatches with duplicate Namespace should not error: %v", err)
	}
	if !strings.Contains(string(result), "replicas: 3") {
		t.Error("patch should be applied despite duplicate resource IDs in input")
	}
}

func TestApplyPatches_FromFile(t *testing.T) {
	// Create a temp patch file.
	baseDir := t.TempDir()
	patchFile := baseDir + "/patch.yaml"
	os.WriteFile(patchFile, []byte(`- op: replace
  path: /spec/replicas
  value: 7
`), 0644)

	resources := []byte(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: podinfo
  namespace: default
spec:
  replicas: 1
`)
	patches := []PatchSpec{
		{
			Target: &PatchTarget{Kind: "Deployment", Name: "podinfo"},
			Path:   "patch.yaml", // relative to baseDir
		},
	}

	result, err := ApplyPatches(resources, patches, baseDir)
	if err != nil {
		t.Fatalf("ApplyPatches from file: %v", err)
	}
	if !strings.Contains(string(result), "replicas: 7") {
		t.Errorf("expected replicas: 7 from file-based patch:\n%s", string(result))
	}
}

func TestApplyPatches_PathTraversalRejected(t *testing.T) {
	baseDir := t.TempDir()
	// Write a file outside baseDir.
	outsideDir := t.TempDir()
	outsideFile := outsideDir + "/secret.txt"
	os.WriteFile(outsideFile, []byte("sensitive"), 0644)

	resources := []byte(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: podinfo
spec:
  replicas: 1
`)
	relOutside, _ := filepath.Rel(baseDir, outsideFile)
	patches := []PatchSpec{
		{
			Target: &PatchTarget{Kind: "Deployment", Name: "podinfo"},
			Path:   relOutside,
		},
	}

	_, err := ApplyPatches(resources, patches, baseDir)
	if err == nil {
		t.Fatal("expected error for path traversal, got nil")
	}
}

// namespaceOf parses multi-doc YAML and returns the metadata.namespace of the
// document matching the given kind/name, plus whether it was found.
func namespaceOf(t *testing.T, data []byte, kind, name string) (string, bool) {
	t.Helper()
	for _, doc := range yamlutil.SplitYAMLText(data) {
		var m struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name      string `yaml:"name"`
				Namespace string `yaml:"namespace"`
			} `yaml:"metadata"`
		}
		if yaml.Unmarshal([]byte(doc), &m) != nil {
			continue
		}
		if m.Kind == kind && m.Metadata.Name == name {
			return m.Metadata.Namespace, true
		}
	}
	return "", false
}

func TestApplyTargetNamespace_Basic(t *testing.T) {
	resources := []byte(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: app
spec:
  replicas: 1
`)
	result, err := ApplyTargetNamespace(resources, "team-a")
	if err != nil {
		t.Fatalf("ApplyTargetNamespace: %v", err)
	}
	ns, ok := namespaceOf(t, result, "Deployment", "app")
	if !ok {
		t.Fatalf("Deployment app not found in output:\n%s", string(result))
	}
	if ns != "team-a" {
		t.Errorf("namespace = %q, want %q", ns, "team-a")
	}
}

func TestApplyTargetNamespace_OverridesExisting(t *testing.T) {
	resources := []byte(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: app
  namespace: default
spec:
  replicas: 1
`)
	result, err := ApplyTargetNamespace(resources, "team-a")
	if err != nil {
		t.Fatalf("ApplyTargetNamespace: %v", err)
	}
	ns, ok := namespaceOf(t, result, "Deployment", "app")
	if !ok {
		t.Fatalf("Deployment app not found in output:\n%s", string(result))
	}
	if ns != "team-a" {
		t.Errorf("namespace = %q, want targetNamespace to override to %q", ns, "team-a")
	}
}

func TestApplyTargetNamespace_ClusterScopedSkipped(t *testing.T) {
	// ClusterRole is cluster-scoped: its namespace must NOT be set.
	resources := []byte(`apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: app-role
rules:
- apiGroups: [""]
  resources: ["pods"]
  verbs: ["get"]
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: app
spec:
  replicas: 1
`)
	result, err := ApplyTargetNamespace(resources, "team-a")
	if err != nil {
		t.Fatalf("ApplyTargetNamespace: %v", err)
	}
	crNS, ok := namespaceOf(t, result, "ClusterRole", "app-role")
	if !ok {
		t.Fatalf("ClusterRole app-role not found in output:\n%s", string(result))
	}
	if crNS != "" {
		t.Errorf("cluster-scoped ClusterRole namespace = %q, want empty (not namespaced)", crNS)
	}
	depNS, ok := namespaceOf(t, result, "Deployment", "app")
	if !ok {
		t.Fatalf("Deployment app not found in output:\n%s", string(result))
	}
	if depNS != "team-a" {
		t.Errorf("Deployment namespace = %q, want %q", depNS, "team-a")
	}
}

func TestApplyTargetNamespace_EmptyNamespace(t *testing.T) {
	resources := []byte(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: app
  namespace: default
`)
	result, err := ApplyTargetNamespace(resources, "")
	if err != nil {
		t.Fatalf("ApplyTargetNamespace with empty namespace: %v", err)
	}
	if string(result) != string(resources) {
		t.Error("empty namespace should return resources unchanged")
	}
}

// firstContainerImage returns the first container image in the first
// workload-like document, parsed out of multi-doc YAML.
func firstContainerImage(t *testing.T, data []byte) string {
	t.Helper()
	for _, doc := range yamlutil.SplitYAMLText(data) {
		var m struct {
			Spec struct {
				Template struct {
					Spec struct {
						Containers []struct {
							Image string `yaml:"image"`
						} `yaml:"containers"`
					} `yaml:"spec"`
				} `yaml:"template"`
			} `yaml:"spec"`
		}
		if yaml.Unmarshal([]byte(doc), &m) != nil {
			continue
		}
		if len(m.Spec.Template.Spec.Containers) > 0 {
			return m.Spec.Template.Spec.Containers[0].Image
		}
	}
	t.Fatalf("no container image found in output:\n%s", string(data))
	return ""
}

func TestApplyImages_NewTag(t *testing.T) {
	resources := []byte(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: app
spec:
  template:
    spec:
      containers:
        - name: app
          image: ghcr.io/stefanprodan/podinfo:5.0.0
`)
	images := []ImageOverride{
		{Name: "ghcr.io/stefanprodan/podinfo", NewTag: "6.0.0"},
	}
	result, err := ApplyImages(resources, images)
	if err != nil {
		t.Fatalf("ApplyImages: %v", err)
	}
	got := firstContainerImage(t, result)
	want := "ghcr.io/stefanprodan/podinfo:6.0.0"
	if got != want {
		t.Errorf("image = %q, want %q", got, want)
	}
}

func TestApplyImages_NewName(t *testing.T) {
	resources := []byte(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: app
spec:
  template:
    spec:
      containers:
        - name: app
          image: podinfo:5.0.0
`)
	images := []ImageOverride{
		{Name: "podinfo", NewName: "ghcr.io/stefanprodan/podinfo", NewTag: "6.0.0"},
	}
	result, err := ApplyImages(resources, images)
	if err != nil {
		t.Fatalf("ApplyImages: %v", err)
	}
	got := firstContainerImage(t, result)
	want := "ghcr.io/stefanprodan/podinfo:6.0.0"
	if got != want {
		t.Errorf("image = %q, want %q", got, want)
	}
}

func TestApplyImages_Digest(t *testing.T) {
	resources := []byte(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: app
spec:
  template:
    spec:
      containers:
        - name: app
          image: podinfo:5.0.0
`)
	digest := "sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	images := []ImageOverride{
		{Name: "podinfo", Digest: digest},
	}
	result, err := ApplyImages(resources, images)
	if err != nil {
		t.Fatalf("ApplyImages: %v", err)
	}
	got := firstContainerImage(t, result)
	want := "podinfo@" + digest
	if got != want {
		t.Errorf("image = %q, want %q", got, want)
	}
}

func TestApplyImages_Empty(t *testing.T) {
	resources := []byte(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: app
`)
	result, err := ApplyImages(resources, nil)
	if err != nil {
		t.Fatalf("ApplyImages with nil images: %v", err)
	}
	if string(result) != string(resources) {
		t.Error("empty images should return resources unchanged")
	}
}
