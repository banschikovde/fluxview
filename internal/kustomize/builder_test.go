package kustomize

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTestFile writes a file, creating parent directories.
func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}

// TestRestrictedFs_BlocksPathTraversal verifies that a kustomization.yaml
// referencing a file outside rootDir via resources: ../../outside.yaml is
// rejected by the restricted filesystem wrapper.
//
// This is a regression test for the LoadRestrictionsNone vulnerability:
// without restrictedFs, kustomize would happily read /etc/passwd or any
// other file outside the repository.
func TestRestrictedFs_BlocksPathTraversal(t *testing.T) {
	rootDir := t.TempDir()

	// A file OUTSIDE rootDir.
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "secret.yaml")
	writeTestFile(t, outsideFile, `apiVersion: v1
kind: Secret
metadata:
  name: leaked
data:
  key: dGVzdA==
`)

	// kustomization inside rootDir referencing the outside file via relative path.
	overlayDir := filepath.Join(rootDir, "app", "base")
	relOutside, err := filepath.Rel(overlayDir, outsideFile)
	if err != nil {
		t.Fatalf("filepath.Rel: %v", err)
	}
	writeTestFile(t, filepath.Join(overlayDir, "kustomization.yaml"),
		"apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - "+relOutside+"\n")

	builder := NewBuilder(rootDir)
	_, err = builder.Build(context.Background(), overlayDir)
	if err == nil {
		t.Fatal("expected error for path traversal, got nil — restrictedFs is not blocking external reads")
	}
	if !strings.Contains(err.Error(), "outside repository root") {
		t.Fatalf("expected 'outside repository root' error, got: %v", err)
	}
}

// TestRestrictedFs_AllowsIntraRepoReference verifies that legitimate
// ../../base references within the repo still work.
func TestRestrictedFs_AllowsIntraRepoReference(t *testing.T) {
	rootDir := t.TempDir()

	// Shared base inside rootDir.
	baseDir := filepath.Join(rootDir, "apps", "base")
	writeTestFile(t, filepath.Join(baseDir, "cm.yaml"),
		`apiVersion: v1
kind: ConfigMap
metadata:
  name: shared
`)
	writeTestFile(t, filepath.Join(baseDir, "kustomization.yaml"),
		`apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - cm.yaml
`)

	// Overlay referencing ../../apps/base from within rootDir.
	overlayDir := filepath.Join(rootDir, "clusters", "test", "app")
	writeTestFile(t, filepath.Join(overlayDir, "kustomization.yaml"),
		`apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../../../apps/base
`)

	builder := NewBuilder(rootDir)
	output, err := builder.Build(context.Background(), overlayDir)
	if err != nil {
		t.Fatalf("intra-repo reference should work, got error: %v", err)
	}
	if !strings.Contains(string(output), "shared") {
		t.Errorf("expected ConfigMap 'shared' in output, got: %s", string(output))
	}
}

// TestRestrictedFs_BlocksSymlinkEscape verifies that a symlink inside rootDir
// pointing to a file outside rootDir is rejected.
func TestRestrictedFs_BlocksSymlinkEscape(t *testing.T) {
	// Skip on systems without symlink support.
	rootDir := t.TempDir()

	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "target.yaml")
	writeTestFile(t, outsideFile, "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: leaked\n")

	// Create symlink inside rootDir pointing outside.
	linkPath := filepath.Join(rootDir, "link.yaml")
	if err := os.Symlink(outsideFile, linkPath); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	fs := newRestrictedFs(rootDir)

	// Reading via symlink should be blocked.
	_, err := fs.ReadFile(linkPath)
	if err == nil {
		t.Error("expected error reading symlink that escapes rootDir")
	}
}
