package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/banschikovde/fluxview/internal/flux"
)

func TestResolveSourcePath_TraversalClamped(t *testing.T) {
	repoRoot := t.TempDir()
	ks := flux.Kustomization{Spec: flux.KustomizationSpec{Path: "../../../../etc/passwd"}}
	got := resolveSourcePath(repoRoot, ks)
	if !strings.HasPrefix(got, repoRoot) {
		t.Errorf("resolved path escaped repoRoot: %s", got)
	}
}

func TestResolveSourcePath_EmptyPath(t *testing.T) {
	repoRoot := t.TempDir()
	ks := flux.Kustomization{Spec: flux.KustomizationSpec{Path: ""}}
	got := resolveSourcePath(repoRoot, ks)
	if got != "" {
		t.Errorf("expected empty string for empty spec.path, got %q", got)
	}
}

func TestResolveSourcePath_NormalPath(t *testing.T) {
	repoRoot := t.TempDir()
	validDir := filepath.Join(repoRoot, "valid")
	if err := os.MkdirAll(validDir, 0755); err != nil {
		t.Fatalf("failed to create valid directory: %v", err)
	}

	ks := flux.Kustomization{Spec: flux.KustomizationSpec{Path: "valid"}}
	got := resolveSourcePath(repoRoot, ks)
	if !strings.HasPrefix(got, repoRoot) {
		t.Errorf("resolved path escaped repoRoot: %s", got)
	}
	if !strings.Contains(got, "valid") {
		t.Errorf("expected path to contain 'valid', got %s", got)
	}
}

func TestCollectKustomizationPaths_IncludesValidPaths(t *testing.T) {
	repoRoot := t.TempDir()

	// Create valid directories
	for _, name := range []string{"apps", "monitoring"} {
		path := filepath.Join(repoRoot, name)
		if err := os.MkdirAll(path, 0755); err != nil {
			t.Fatalf("failed to create directory %s: %v", name, err)
		}
	}

	kustomizations := []flux.Kustomization{
		{Metadata: flux.ObjectMeta{Name: "test-1"}, Spec: flux.KustomizationSpec{Path: "apps"}},
		{Metadata: flux.ObjectMeta{Name: "test-2"}, Spec: flux.KustomizationSpec{Path: "monitoring"}},
		{Metadata: flux.ObjectMeta{Name: "test-3"}, Spec: flux.KustomizationSpec{Path: ""}},
	}
	paths := collectKustomizationPaths(repoRoot, kustomizations)
	if len(paths) != 2 {
		t.Errorf("expected 2 resolved paths, got %d", len(paths))
	}

	// Verify both paths are within repoRoot
	for path := range paths {
		if !strings.HasPrefix(path, repoRoot) {
			t.Errorf("resolved path escaped repoRoot: %s", path)
		}
	}
}