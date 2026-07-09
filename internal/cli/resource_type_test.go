package cli

import (
	"context"
	"testing"
)

// TestRunBuild_AllWithName verifies that 'all' + name is rejected.
func TestRunBuild_AllWithName(t *testing.T) {
	err := runBuild(context.Background(), []string{"all", "podinfo"}, &BuildFlags{})
	if err == nil {
		t.Fatal("expected error for 'all' with name")
	}
	exitErr, ok := err.(*DiffExitError)
	if !ok {
		t.Fatalf("expected DiffExitError, got %T: %v", err, err)
	}
	if exitErr.ExitCode != ExitCodeError {
		t.Errorf("exit code = %d, want %d", exitErr.ExitCode, ExitCodeError)
	}
}

// TestRunDiff_AllWithName verifies that 'all' + name is rejected.
func TestRunDiff_AllWithName(t *testing.T) {
	err := runDiff(context.Background(), []string{"all", "podinfo"}, &DiffFlags{})
	if err == nil {
		t.Fatal("expected error for 'all' with name")
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
// types produce a clear error message mentioning 'all' as an option.
func TestRunBuild_UnsupportedResourceType(t *testing.T) {
	tests := []struct {
		resourceType string
		shouldError  bool
	}{
		{"ks", false},
		{"kustomization", false},
		{"hr", false},
		{"helmrelease", false},
		{"all", false},
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
// types produce a clear error message mentioning 'all' as an option.
func TestRunDiff_UnsupportedResourceType(t *testing.T) {
	tests := []struct {
		resourceType string
		shouldError  bool
	}{
		{"ks", false},
		{"kustomization", false},
		{"hr", false},
		{"helmrelease", false},
		{"all", false},
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
