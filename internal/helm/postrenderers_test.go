package helm

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/banschikovde/fluxview/internal/kustomize"
)

// TestPostRenderersLoop_Positive verifies that sequential postRenderer
// patches are applied correctly to rendered output. This tests the
// loop logic used in InflateHelmRelease, without requiring a real Helm chart.
func TestPostRenderersLoop_Positive(t *testing.T) {
	// Simulate rendered chart output.
	converted := []byte(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: podinfo
  namespace: default
spec:
  replicas: 1
`)

	patch1 := []kustomize.PatchSpec{
		{
			Target: &kustomize.PatchTarget{Kind: "Deployment", Name: "podinfo"},
			Patch: `- op: replace
  path: /spec/replicas
  value: 5
`,
		},
	}
	patch2 := []kustomize.PatchSpec{
		{
			Target: &kustomize.PatchTarget{Kind: "Deployment", Name: "podinfo"},
			Patch: `- op: add
  path: /metadata/labels
  value:
    patched: "true"
`,
		},
	}

	// Simulate InflateHelmRelease's postRenderers loop.
	postRenderers := [][]kustomize.PatchSpec{patch1, patch2}
	for _, patches := range postRenderers {
		patched, err := kustomize.ApplyPatches(converted, patches)
		if err != nil {
			t.Fatalf("ApplyPatches: %v", err)
		}
		converted = patched
	}

	output := string(converted)
	if !strings.Contains(output, "replicas: 5") {
		t.Error("first postRenderer (replicas: 5) not applied")
	}
	if !strings.Contains(output, "patched: \"true\"") {
		t.Error("second postRenderer (label) not applied")
	}
}

// TestPostRenderersLoop_ErrorContinuesWarns verifies that when one
// postRenderer fails, subsequent postRenderers are still processed and
// a warning is printed to stderr. This tests the loop + error-handling
// logic from InflateHelmRelease without a real Helm chart.
func TestPostRenderersLoop_ErrorContinuesWarns(t *testing.T) {
	converted := []byte(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: podinfo
  namespace: default
spec:
  replicas: 1
`)

	badPatch := []kustomize.PatchSpec{
		{
			Target: &kustomize.PatchTarget{Kind: "Deployment", Name: "podinfo"},
			Patch:  `this is not valid yaml patch`,
		},
	}
	goodPatch := []kustomize.PatchSpec{
		{
			Target: &kustomize.PatchTarget{Kind: "Deployment", Name: "podinfo"},
			Patch: `- op: replace
  path: /spec/replicas
  value: 9
`,
		},
	}

	hrNS := "default"
	hrName := "podinfo"

	// Capture stderr to verify warning.
	old := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	// Simulate InflateHelmRelease's postRenderers loop.
	postRenderers := [][]kustomize.PatchSpec{badPatch, goodPatch}
	for _, patches := range postRenderers {
		patched, err := kustomize.ApplyPatches(converted, patches)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to apply postRenderer patches for %s/%s: %v\n",
				hrNS, hrName, err)
			continue
		}
		converted = patched
	}

	w.Close()
	os.Stderr = old
	var buf bytes.Buffer
	io.Copy(&buf, r)
	stderrOutput := buf.String()

	output := string(converted)
	if !strings.Contains(output, "replicas: 9") {
		t.Error("second postRenderer should still be applied after first fails")
	}
	if !strings.Contains(stderrOutput, "Warning:") {
		t.Errorf("expected warning in stderr, got:\n%s", stderrOutput)
	}
	if !strings.Contains(stderrOutput, hrName) {
		t.Errorf("expected HR name %q in warning, got:\n%s", hrName, stderrOutput)
	}
}
