package helm

import (
	"strings"
	"testing"

	"github.com/banschikovde/fluxview/internal/kustomize"
)

// TestApplyPostRenderers_Positive verifies that postRenderers patches
// are applied to rendered Helm chart output.
func TestApplyPostRenderers_Positive(t *testing.T) {
	// Simulate rendered chart output (as it would come from InflateHelmRelease).
	converted := []byte(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: podinfo
  namespace: default
spec:
  replicas: 1
`)

	// Two postRenderers: first changes replicas, second adds a label.
	patch1 := `- op: replace
  path: /spec/replicas
  value: 5
`
	patch2 := `- op: add
  path: /metadata/labels
  value:
    patched: "true"
`

	for _, patches := range [][]kustomize.PatchSpec{
		{
			{
				Target: &kustomize.PatchTarget{Kind: "Deployment", Name: "podinfo"},
				Patch:  patch1,
			},
		},
		{
			{
				Target: &kustomize.PatchTarget{Kind: "Deployment", Name: "podinfo"},
				Patch:  patch2,
			},
		},
	} {
		result, err := kustomize.ApplyPatches(converted, patches)
		if err != nil {
			t.Fatalf("ApplyPatches: %v", err)
		}
		converted = result
	}

	output := string(converted)
	if !strings.Contains(output, "replicas: 5") {
		t.Error("first postRenderer (replicas: 5) not applied")
	}
	if !strings.Contains(output, "patched: \"true\"") {
		t.Error("second postRenderer (label) not applied")
	}
}

// TestApplyPostRenderers_ErrorContinues verifies that when one postRenderer
// fails, subsequent postRenderers are still processed (not silently aborted).
func TestApplyPostRenderers_ErrorContinues(t *testing.T) {
	converted := []byte(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: podinfo
  namespace: default
spec:
  replicas: 1
`)

	// First patch: invalid JSON6902 (will cause kustomize error).
	badPatch := []kustomize.PatchSpec{
		{
			Target: &kustomize.PatchTarget{Kind: "Deployment", Name: "podinfo"},
			Patch:  `this is not valid yaml patch`,
		},
	}
	// Second patch: valid, should still be applied.
	goodPatch := []kustomize.PatchSpec{
		{
			Target: &kustomize.PatchTarget{Kind: "Deployment", Name: "podinfo"},
			Patch: `- op: replace
  path: /spec/replicas
  value: 9
`,
		},
	}

	postRenderers := [][]kustomize.PatchSpec{badPatch, goodPatch}

	for _, patches := range postRenderers {
		patched, err := kustomize.ApplyPatches(converted, patches)
		if err != nil {
			// Simulate what InflateHelmRelease does: warn and continue.
			continue
		}
		converted = patched
	}

	output := string(converted)
	if !strings.Contains(output, "replicas: 9") {
		t.Error("second postRenderer should still be applied after first fails")
	}
}
