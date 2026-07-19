package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitInit runs `git init` in dir to make it discoverable by FindRepoRoot.
func gitInit(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.Command("git", "init", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init %s: %v\n%s", dir, err, out)
	}
}

// TestRunValidate_RelativePath_NoWarnings is a regression test for the bug
// where `fluxview validate --path <relative>` produced spurious warnings of
// the form:
//
//	Warning: could not read cluster/ks.yaml: Rel: can't make cluster/ks.yaml
//	relative to /Users/…/repo
//
// Root cause: runValidate passed the raw (relative) clusterPath into
// flux.NewParser / resolveConfigMaps / buildKSContent instead of
// absClusterPath. The loose-file walker in buildKustomizeOverlays then
// called filepath.Rel(repoRoot, relativePath), which fails because the
// second argument isn't absolute.
//
// After the fix, all downstream calls receive absClusterPath and the
// loose-file walker resolves paths correctly.
func TestRunValidate_RelativePath_NoWarnings(t *testing.T) {
	repoRoot := t.TempDir()
	gitInit(t, repoRoot)

	// A cluster directory holding a Flux Kustomization CR.
	clusterDir := filepath.Join(repoRoot, "cluster")
	writeHelper(t, clusterDir, "ks.yaml", `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: app
  namespace: flux-system
spec:
  interval: 5m
  path: ./app
  prune: true
  sourceRef:
    kind: GitRepository
    name: flux-system
`)

	// The spec.path target with a native kustomization and one resource.
	appDir := filepath.Join(repoRoot, "app")
	writeHelper(t, appDir, "kustomization.yaml", `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - configmap.yaml
`)
	writeHelper(t, appDir, "configmap.yaml", `apiVersion: v1
kind: ConfigMap
metadata:
  name: app-config
data:
  key: value
`)

	// Empty schema dir → 0 schemas loaded, resources silently skipped,
	// validate exits 0 with "All resources valid."
	schemaDir := filepath.Join(repoRoot, "crds")
	if err := os.MkdirAll(schemaDir, 0755); err != nil {
		t.Fatalf("mkdir schema dir: %v", err)
	}

	// Run validate from repoRoot with a RELATIVE --path. chdir back afterwards.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getcwd: %v", err)
	}
	defer func() { _ = os.Chdir(cwd) }()
	if err := os.Chdir(repoRoot); err != nil {
		t.Fatalf("chdir %s: %v", repoRoot, err)
	}

	flags := &ValidateFlags{
		Path:      "cluster", // relative on purpose
		SchemaDir: schemaDir,
	}

	stderr := captureStderr(func() {
		_ = runValidate(context.Background(), flags)
	})

	if strings.Contains(stderr, "Rel: can't make") {
		t.Errorf("validate with relative --path produced spurious Rel warnings:\n%s", stderr)
	}
	// Sanity: the rest of the flow should still work (build runs, no schemas,
	// "All resources valid.").
	if !strings.Contains(stderr, "All resources valid.") {
		t.Errorf("expected 'All resources valid.' in stderr, got:\n%s", stderr)
	}
}
