// Package git provides local git operations for fluxview using the go-git SDK.
package git

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// Operations provides git operations for the local repository via go-git SDK.
type Operations struct {
	repo     *git.Repository
	RepoRoot string
}

// NewOperations creates a new git Operations instance using the go-git SDK.
func NewOperations(repoRoot string) (*Operations, error) {
	repo, err := git.PlainOpen(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("opening git repo at %s: %w", repoRoot, err)
	}

	return &Operations{
		repo:     repo,
		RepoRoot: repoRoot,
	}, nil
}

// DefaultBranch returns the default branch name (main or master).
func (g *Operations) DefaultBranch(_ context.Context) (string, error) {
	// Try to determine from refs/remotes/origin/HEAD symbolic ref.
	ref, err := g.repo.Reference(plumbing.ReferenceName("refs/remotes/origin/HEAD"), false)
	if err == nil {
		target := ref.Target()
		if target != "" {
			// target is like "refs/remotes/origin/main"
			parts := strings.Split(string(target), "/")
			if len(parts) > 0 {
				return parts[len(parts)-1], nil
			}
		}
	}

	// Fallback: check if main or master exists.
	for _, branch := range []string{"main", "master"} {
		_, err := g.repo.Reference(plumbing.ReferenceName("refs/heads/"+branch), false)
		if err == nil {
			return branch, nil
		}
	}

	return "", fmt.Errorf("could not determine default branch")
}

// ResolveRevision resolves a revision string to a commit hash.
func (g *Operations) ResolveRevision(_ context.Context, revision string) (string, error) {
	hash, err := g.repo.ResolveRevision(plumbing.Revision(revision))
	if err != nil {
		return "", fmt.Errorf("resolving revision %s: %w", revision, err)
	}
	return hash.String(), nil
}

// CloneToDir checks out all files at a specific revision into a temp directory.
// Returns the temp directory path (caller should clean up).
func (g *Operations) CloneToDir(_ context.Context, revision string) (string, error) {
	hash, err := g.repo.ResolveRevision(plumbing.Revision(revision))
	if err != nil {
		return "", fmt.Errorf("resolving revision %s: %w", revision, err)
	}

	commit, err := g.repo.CommitObject(*hash)
	if err != nil {
		return "", fmt.Errorf("getting commit %s: %w", revision, err)
	}

	tree, err := commit.Tree()
	if err != nil {
		return "", fmt.Errorf("getting tree for %s: %w", revision, err)
	}

	tmpDir, err := os.MkdirTemp("", "fluxview-revision-*")
	if err != nil {
		return "", fmt.Errorf("creating temp dir: %w", err)
	}

	// Write all files from the tree into the temp directory.
	err = tree.Files().ForEach(func(f *object.File) error {
		filePath := filepath.Join(tmpDir, f.Name)
		if err := os.MkdirAll(filepath.Dir(filePath), 0755); err != nil {
			return fmt.Errorf("creating directory %s: %w", filepath.Dir(filePath), err)
		}

		// Handle symlinks: git stores the target path as file content.
		if f.Mode == filemode.Symlink {
			reader, err := f.Reader()
			if err != nil {
				return fmt.Errorf("opening symlink %s: %w", f.Name, err)
			}
			target, err := io.ReadAll(reader)
			reader.Close()
			if err != nil {
				return fmt.Errorf("reading symlink %s: %w", f.Name, err)
			}
			linkTarget := strings.TrimSpace(string(target))

			// Validate that the symlink stays within tmpDir to prevent
			// reading arbitrary files (e.g. /etc/passwd) via malicious commits.
			var resolved string
			if filepath.IsAbs(linkTarget) {
				resolved = linkTarget
			} else {
				resolved = filepath.Join(filepath.Dir(filePath), linkTarget)
			}
			absResolved, err := filepath.Abs(resolved)
			if err != nil {
				return fmt.Errorf("resolving symlink %s: %w", f.Name, err)
			}
			if !isWithinDir(absResolved, tmpDir) {
				fmt.Fprintf(os.Stderr, "Warning: skipping symlink %s -> %s (target outside checkout)\n", f.Name, linkTarget)
				return nil
			}

			return os.Symlink(linkTarget, filePath)
		}

		reader, err := f.Reader()
		if err != nil {
			return fmt.Errorf("opening file %s: %w", f.Name, err)
		}

		contents, err := io.ReadAll(reader)
		reader.Close()
		if err != nil {
			return fmt.Errorf("reading file %s: %w", f.Name, err)
		}

		if err := os.WriteFile(filePath, contents, os.FileMode(f.Mode)); err != nil {
			return fmt.Errorf("writing file %s: %w", f.Name, err)
		}
		return nil
	})

	if err != nil {
		os.RemoveAll(tmpDir)
		return "", fmt.Errorf("checking out files at %s: %w", revision, err)
	}

	return tmpDir, nil
}

// RemoveWorktree removes a previously created worktree/checkout directory.
func (g *Operations) RemoveWorktree(_ context.Context, worktreePath string) error {
	return os.RemoveAll(worktreePath)
}

// FindRepoRoot searches upward from the given path to find the git repository root.
func FindRepoRoot(startPath string) (string, error) {
	path, err := filepath.Abs(startPath)
	if err != nil {
		return "", fmt.Errorf("resolving path %s: %w", startPath, err)
	}

	for {
		gitDir := filepath.Join(path, ".git")
		if _, err := os.Stat(gitDir); err == nil {
			return path, nil
		}

		parent := filepath.Dir(path)
		if parent == path {
			return "", fmt.Errorf("not a git repository (or any parent): %s", startPath)
		}
		path = parent
	}
}

// isWithinDir checks if path stays within dir after resolution.
// Both path and dir should be absolute.
func isWithinDir(path, dir string) bool {
	return path == dir ||
		strings.HasPrefix(path+string(filepath.Separator), dir+string(filepath.Separator))
}
