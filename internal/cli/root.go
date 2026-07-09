// Package cli provides the Cobra-based CLI for fluxview.
package cli

import (
	"errors"
	"fmt"
	"os"

	diffpkg "github.com/banschikovde/fluxview/internal/diff"
	"github.com/spf13/cobra"
)

const (
	// ExitDiffFound indicates successful execution with differences found.
	ExitDiffFound = 1
	// ExitValidationFailed indicates validation found invalid resources.
	ExitValidationFailed = 3
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
	var exitErr *DiffExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode
	}
	return ExitCodeError
}

// version is set at build time via ldflags:
//
//	-ldflags "-X github.com/banschikovde/fluxview/internal/cli.version=v1.0.0"
var version = "dev"

// Version returns the current build version.
func Version() string {
	return version
}

// NewRootCmd creates the root command for fluxview.
func NewRootCmd() *cobra.Command {
	rootCmd := &cobra.Command{
		Use:   "fluxview",
		Short: "CLI tool for building, diffing, and validating Flux GitOps resources locally",
		Long: `fluxview is a CLI tool that builds, diffs, and validates Flux GitOps
resources from a local git repository. It orchestrates kustomize and helm
to replicate Flux behavior without connecting to a live cluster.

Commands:
  build     Build (assemble) Kustomization or HelmRelease resources
  diff      Compare Kustomization or HelmRelease resources against another git revision
  validate  Validate resources against bundled CRD schemas`,
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	rootCmd.AddCommand(newBuildCmd())
	rootCmd.AddCommand(newDiffCmd())
	rootCmd.AddCommand(newValidateCmd())

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
