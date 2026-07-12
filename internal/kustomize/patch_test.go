package kustomize

import (
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

	result, err := ApplyPatches(resources, patches)
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

	result, err := ApplyPatches(resources, patches)
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
	result, err := ApplyPatches(resources, nil)
	if err != nil {
		t.Fatalf("ApplyPatches with nil patches: %v", err)
	}
	if string(result) != string(resources) {
		t.Error("empty patches should return resources unchanged")
	}
}
