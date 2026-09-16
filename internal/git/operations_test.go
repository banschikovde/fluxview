package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func TestIsWithinDir(t *testing.T) {
	dir := "/tmp/checkout"

	tests := []struct {
		name string
		path string
		dir  string
		want bool
	}{
		{"path inside dir", "/tmp/checkout/file.yaml", dir, true},
		{"path in subdir", "/tmp/checkout/app/config.yaml", dir, true},
		{"path is dir itself", dir, dir, true},
		{"path outside dir", "/tmp/other/file.yaml", dir, false},
		{"path traversal", "/tmp/checkout-evil/file.yaml", dir, false},
		{"absolute escape", "/etc/passwd", dir, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isWithinDir(tt.path, tt.dir); got != tt.want {
				t.Errorf("isWithinDir(%q, %q) = %v, want %v", tt.path, tt.dir, got, tt.want)
			}
		})
	}
}

func TestOriginURL(t *testing.T) {
	ctx := context.Background()

	newRepoWithRemotes := func(t *testing.T, remotes ...*config.RemoteConfig) *Operations {
		t.Helper()
		dir := t.TempDir()
		repo, err := git.PlainInit(dir, false)
		if err != nil {
			t.Fatalf("PlainInit: %v", err)
		}
		for _, cfg := range remotes {
			if _, err := repo.CreateRemote(cfg); err != nil {
				t.Fatalf("CreateRemote %s: %v", cfg.Name, err)
			}
		}
		ops, err := NewOperations(dir)
		if err != nil {
			t.Fatalf("NewOperations: %v", err)
		}
		return ops
	}

	remote := func(name, url string) *config.RemoteConfig {
		return &config.RemoteConfig{Name: name, URLs: []string{url}}
	}

	t.Run("origin preferred over other remotes", func(t *testing.T) {
		ops := newRepoWithRemotes(t,
			remote("upstream", "https://github.com/fluxcd/flux2.git"),
			remote("origin", "git@github.com:org/cluster.git"))
		got, err := ops.OriginURL(ctx)
		if err != nil {
			t.Fatalf("OriginURL: %v", err)
		}
		if got != "git@github.com:org/cluster.git" {
			t.Errorf("OriginURL = %q, want the origin URL", got)
		}
	})

	t.Run("single remote when no origin", func(t *testing.T) {
		ops := newRepoWithRemotes(t, remote("upstream", "https://github.com/fluxcd/flux2.git"))
		got, err := ops.OriginURL(ctx)
		if err != nil {
			t.Fatalf("OriginURL: %v", err)
		}
		if got != "https://github.com/fluxcd/flux2.git" {
			t.Errorf("OriginURL = %q, want the single remote URL", got)
		}
	})

	t.Run("several remotes without origin are ambiguous", func(t *testing.T) {
		ops := newRepoWithRemotes(t,
			remote("upstream", "https://github.com/fluxcd/flux2.git"),
			remote("fork", "https://github.com/org/flux2.git"))
		if _, err := ops.OriginURL(ctx); err == nil {
			t.Fatal("OriginURL with several non-origin remotes must fail")
		}
	})

	t.Run("no remotes configured", func(t *testing.T) {
		ops := newRepoWithRemotes(t)
		if _, err := ops.OriginURL(ctx); err == nil {
			t.Fatal("OriginURL on a repo without remotes must fail")
		}
	})

	t.Run("cancelled context", func(t *testing.T) {
		ops := newRepoWithRemotes(t, remote("origin", "https://github.com/org/cluster.git"))
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := ops.OriginURL(cancelled); err == nil {
			t.Fatal("OriginURL with a cancelled context must fail")
		}
	})
}

// TestCloneToDir_SymlinkEscape creates a real git repo with a malicious
// symlink (absolute target /etc/passwd), commits it, then calls CloneToDir
// and verifies the symlink is NOT present in the checkout.
func TestCloneToDir_SymlinkEscape(t *testing.T) {
	repoDir := t.TempDir()
	repo, err := git.PlainInit(repoDir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}

	// Create a symlink in the worktree pointing to an absolute path outside.
	linkPath := filepath.Join(repoDir, "escape.yaml")
	if err := os.Symlink("/etc/passwd", linkPath); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	// Stage and commit the symlink.
	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if _, err := w.Add("escape.yaml"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	commitHash, err := w.Commit("malicious symlink", &git.CommitOptions{
		Author: &object.Signature{Name: "test", Email: "test@test.com"},
	})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// CloneToDir should succeed (symlink is skipped, not fatal).
	ops, err := NewOperations(repoDir)
	if err != nil {
		t.Fatalf("NewOperations: %v", err)
	}

	tmpDir, err := ops.CloneToDir(context.Background(), commitHash.String())
	if err != nil {
		t.Fatalf("CloneToDir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// The malicious symlink must NOT exist in the checkout.
	linkInCheckout := filepath.Join(tmpDir, "escape.yaml")
	if _, err := os.Lstat(linkInCheckout); !os.IsNotExist(err) {
		t.Errorf("malicious symlink should not exist in checkout, got err: %v", err)
	}
}

// TestCloneToDir_LegitimateSymlink verifies that intra-checkout symlinks
// are preserved by CloneToDir.
func TestCloneToDir_LegitimateSymlink(t *testing.T) {
	repoDir := t.TempDir()
	repo, err := git.PlainInit(repoDir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}

	// Create a real file and a symlink to it within the repo.
	if err := os.WriteFile(filepath.Join(repoDir, "real.yaml"), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.yaml", filepath.Join(repoDir, "link.yaml")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if _, err := w.Add("real.yaml"); err != nil {
		t.Fatalf("Add real.yaml: %v", err)
	}
	if _, err := w.Add("link.yaml"); err != nil {
		t.Fatalf("Add link.yaml: %v", err)
	}
	commitHash, err := w.Commit("legitimate symlink", &git.CommitOptions{
		Author: &object.Signature{Name: "test", Email: "test@test.com"},
	})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	ops, err := NewOperations(repoDir)
	if err != nil {
		t.Fatalf("NewOperations: %v", err)
	}

	tmpDir, err := ops.CloneToDir(context.Background(), commitHash.String())
	if err != nil {
		t.Fatalf("CloneToDir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// The symlink should exist and resolve to real.yaml.
	linkInCheckout := filepath.Join(tmpDir, "link.yaml")
	info, err := os.Lstat(linkInCheckout)
	if err != nil {
		t.Fatalf("legitimate symlink should exist in checkout: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Error("expected symlink, got regular file")
	}

	// Reading through the symlink should work.
	content, err := os.ReadFile(linkInCheckout)
	if err != nil {
		t.Fatalf("reading through symlink: %v", err)
	}
	if string(content) != "content" {
		t.Errorf("symlink content = %q, want %q", string(content), "content")
	}
}
