// Package fsx holds filesystem helpers shared across fluxview's walkers.
package fsx

import (
	"io"
	"os"
	"path/filepath"
)

// ReadRootFile reads absPath by opening it relative to root, so a symlink
// (or symlink chain) that resolves outside rootPath is rejected instead of
// followed. This closes the TOCTOU / symlink-escape gap (CWE-367) that a
// bare os.ReadFile has inside a filepath.Walk callback. root must have
// been created by os.OpenRoot(rootPath); absPath must be lexically inside
// rootPath. os.Root semantics: relative symlinks that stay inside rootPath
// keep working; a symlink with an absolute target is always rejected (even
// when it points back inside rootPath) — the same boundary the loose-file
// walkers in internal/cli use.
func ReadRootFile(root *os.Root, rootPath, absPath string) ([]byte, error) {
	rel, err := filepath.Rel(rootPath, absPath)
	if err != nil {
		return nil, err
	}
	f, err := root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}
