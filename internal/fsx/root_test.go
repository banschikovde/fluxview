package fsx

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadRootFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file.yaml"), []byte("a: 1\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	rootFS, err := os.OpenRoot(root)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer rootFS.Close()

	t.Run("reads a regular file", func(t *testing.T) {
		data, err := ReadRootFile(rootFS, root, filepath.Join(root, "file.yaml"))
		if err != nil || string(data) != "a: 1\n" {
			t.Errorf("ReadRootFile = %q, %v; want file content", data, err)
		}
	})

	t.Run("reads through a relative intra-root symlink", func(t *testing.T) {
		link := filepath.Join(root, "alias.yaml")
		if err := os.Symlink("file.yaml", link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		data, err := ReadRootFile(rootFS, root, link)
		if err != nil || string(data) != "a: 1\n" {
			t.Errorf("ReadRootFile via relative symlink = %q, %v; want file content", data, err)
		}
	})

	t.Run("rejects an absolute-target symlink even when it points inside the root", func(t *testing.T) {
		// os.Root rejects absolute symlink targets outright; intra-repo
		// symlinks stored by git are relative and keep working.
		link := filepath.Join(root, "abs.yaml")
		if err := os.Symlink(filepath.Join(root, "file.yaml"), link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if _, err := ReadRootFile(rootFS, root, link); err == nil {
			t.Error("ReadRootFile must reject an absolute symlink target (os.Root semantics)")
		}
	})

	t.Run("rejects a symlink escaping the root", func(t *testing.T) {
		outside := t.TempDir()
		outsideFile := filepath.Join(outside, "secret.yaml")
		if err := os.WriteFile(outsideFile, []byte("leak\n"), 0o644); err != nil {
			t.Fatalf("write outside file: %v", err)
		}
		link := filepath.Join(root, "leak.yaml")
		if err := os.Symlink(outsideFile, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if _, err := ReadRootFile(rootFS, root, link); err == nil {
			t.Error("ReadRootFile must reject a symlink resolving outside the root")
		}
	})
}
