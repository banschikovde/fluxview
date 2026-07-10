package git

import (
	"os"
	"path/filepath"
	"testing"
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

// TestCloneToDir_SymlinkEscape verifies that symlinks pointing outside the
// checkout directory are NOT created. A malicious commit could create a
// symlink to /etc/passwd; without validation, subsequent file reads would
// leak arbitrary file contents.
func TestCloneToDir_SymlinkEscape(t *testing.T) {
	// This test validates the isWithinDir logic used by CloneToDir's symlink
	// guard. A full integration test would require a git repo with a malicious
	// symlink commit, which is complex to set up in a unit test.

	tmpDir := t.TempDir()

	// Simulate a symlink target that escapes the checkout.
	escapeTarget := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(escapeTarget, []byte("sensitive"), 0o644); err != nil {
		t.Fatal(err)
	}

	// An absolute path symlink target should be rejected.
	// Simulate how CloneToDir resolves it: absolute targets are used as-is.
	if isWithinDir("/etc/passwd", tmpDir) {
		t.Error("absolute symlink target outside checkout should be rejected")
	}

	// A relative traversal target (../../outside) should also be rejected.
	linkPath := filepath.Join(tmpDir, "subdir", "escape.yaml")
	relTarget := filepath.Join("..", "..", "outside")
	resolved2 := filepath.Join(filepath.Dir(linkPath), relTarget)
	absResolved2, _ := filepath.Abs(resolved2)

	if isWithinDir(absResolved2, tmpDir) {
		t.Error("relative traversal symlink target should be rejected")
	}

	// A legitimate intra-checkout symlink should be allowed.
	linkPath3 := filepath.Join(tmpDir, "subdir", "link.yaml")
	intraTarget := "real.yaml"
	resolved3 := filepath.Join(filepath.Dir(linkPath3), intraTarget)
	absResolved3, _ := filepath.Abs(resolved3)

	if !isWithinDir(absResolved3, tmpDir) {
		t.Error("intra-checkout symlink target should be allowed")
	}
}
