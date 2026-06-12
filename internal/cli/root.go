// Package cli provides the Cobra-based CLI for flux-diff.
package cli

import (
	"fmt"
	"os"

	diffpkg "github.com/banschikovde/flux-diff/internal/diff"
	"github.com/spf13/cobra"
)

const (
	// ExitSuccess indicates successful execution with no differences.
	ExitSuccess = 0
	// ExitDiffFound indicates successful execution with differences found.
	ExitDiffFound = 1
	// ExitCodeError indicates an error occurred.
	ExitCodeError = 2
)

// DiffExitError wraps an error with a specific exit code.
type DiffExitError struct {
	Err      error
	ExitCode int
}

func (e *DiffExitError) Error() string {
	return e.Err.Error()
}

// NewExitError creates a new DiffExitError.
func NewExitError(err error, code int) *DiffExitError {
	return &DiffExitError{Err: err, ExitCode: code}
}

// ExitCodeFromError extracts the exit code from an error.
// Returns ExitCodeError for unknown errors.
func ExitCodeFromError(err error) int {
	if exitErr, ok := err.(*DiffExitError); ok {
		return exitErr.ExitCode
	}
	return ExitCodeError
}

// NewRootCmd creates the root command for flux-diff.
func NewRootCmd() *cobra.Command {
	rootCmd := &cobra.Command{
		Use:   "flux-diff",
		Short: "CLI tool for building and diffing Flux GitOps resources locally",
		Long: `flux-diff is a CLI tool that builds and diffs Flux GitOps resources
from a local git repository. It orchestrates kustomize and helm to replicate
Flux behavior without connecting to a live cluster.

Commands:
  build    Build (assemble) Kustomization or HelmRelease resources
  diff     Compare Kustomization or HelmRelease resources against another git revision`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	rootCmd.AddCommand(newBuildCmd())
	rootCmd.AddCommand(newDiffCmd())

	return rootCmd
}

// resolveColorMode parses and validates the color mode flag.
func resolveColorMode(colorFlag string) diffpkg.ColorMode {
	switch colorFlag {
	case "always":
		return diffpkg.ColorAlways
	case "never":
		return diffpkg.ColorNever
	case "auto":
		return diffpkg.ColorAuto
	default:
		fmt.Fprintf(os.Stderr, "Warning: unknown color mode %q, using 'auto'\n", colorFlag)
		return diffpkg.ColorAuto
	}
}
