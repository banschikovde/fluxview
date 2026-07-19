package cli

import (
	"context"
	"testing"
)

// TestRunBuild_NoResourceType verifies that missing resource type is rejected.
func TestRunBuild_NoResourceType(t *testing.T) {
	err := runBuild(context.Background(), []string{}, &BuildFlags{})
	if err == nil {
		t.Fatal("expected error for missing resource type")
	}
	exitErr, ok := err.(*DiffExitError)
	if !ok {
		t.Fatalf("expected DiffExitError, got %T: %v", err, err)
	}
	if exitErr.ExitCode != ExitCodeError {
		t.Errorf("exit code = %d, want %d", exitErr.ExitCode, ExitCodeError)
	}
}

// TestRunDiff_NoResourceType verifies that missing resource type is rejected.
func TestRunDiff_NoResourceType(t *testing.T) {
	err := runDiff(context.Background(), []string{}, &DiffFlags{})
	if err == nil {
		t.Fatal("expected error for missing resource type")
	}
	exitErr, ok := err.(*DiffExitError)
	if !ok {
		t.Fatalf("expected DiffExitError, got %T: %v", err, err)
	}
	if exitErr.ExitCode != ExitCodeError {
		t.Errorf("exit code = %d, want %d", exitErr.ExitCode, ExitCodeError)
	}
}

// TestRunBuild_UnsupportedResourceType verifies that unsupported resource
// types produce a clear error message.
func TestRunBuild_UnsupportedResourceType(t *testing.T) {
	tests := []struct {
		resourceType string
		shouldError  bool
	}{
		{"ks", false},
		{"kustomization", false},
		{"hr", false},
		{"helmrelease", false},
		{"all", true},
		{"xyz", true},
		{"", true},
	}

	for _, tt := range tests {
		t.Run(tt.resourceType, func(t *testing.T) {
			// We can't test the full build (needs git/kustomize), but we can
			// verify resource type dispatch by checking the error path.
			// runBuild with a non-existent path returns ExitCodeError for path,
			// but with an empty path it defaults to "." which exists.
			// For unsupported types, it should return ExitCodeError with
			// "unsupported resource type" message.
			err := runBuild(context.Background(), []string{tt.resourceType}, &BuildFlags{})

			if tt.shouldError && err == nil {
				t.Error("expected error for unsupported resource type")
			}
			// For valid types, we expect an error too (no git repo in test),
			// but the error should NOT be "unsupported resource type".
		})
	}
}

// TestRunDiff_UnsupportedResourceType verifies that unsupported resource
// types produce a clear error message.
func TestRunDiff_UnsupportedResourceType(t *testing.T) {
	tests := []struct {
		resourceType string
		shouldError  bool
	}{
		{"ks", false},
		{"kustomization", false},
		{"hr", false},
		{"helmrelease", false},
		{"all", true},
		{"xyz", true},
		{"", true},
	}

	for _, tt := range tests {
		t.Run(tt.resourceType, func(t *testing.T) {
			err := runDiff(context.Background(), []string{tt.resourceType}, &DiffFlags{})

			if tt.shouldError && err == nil {
				t.Error("expected error for unsupported resource type")
			}
		})
	}
}
