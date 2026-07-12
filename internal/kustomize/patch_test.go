package kustomize

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
