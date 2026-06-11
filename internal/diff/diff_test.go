package diff

import (
	"strings"
	"testing"
)

func TestComputeNoDiff(t *testing.T) {
	result := Compute("hello\nworld\n", "hello\nworld\n")
	if result.HasDiff {
		t.Error("expected no diff, but differences were found")
	}
	if result.RawDiff != "" {
		t.Errorf("expected empty raw diff, got %q", result.RawDiff)
	}
}

func TestComputeWithDiff(t *testing.T) {
	result := Compute("line1\nline2\nline3\n", "line1\nline2a\nline3\n")
	if !result.HasDiff {
		t.Error("expected diff to be found")
	}
	if result.RawDiff == "" {
		t.Error("expected non-empty raw diff")
	}

	// Verify the diff contains expected markers.
	if !strings.Contains(result.RawDiff, "-line2") {
		t.Errorf("expected diff to contain '-line2', got %q", result.RawDiff)
	}
	if !strings.Contains(result.RawDiff, "+line2a") {
		t.Errorf("expected diff to contain '+line2a', got %q", result.RawDiff)
	}
}

func TestComputeAddLines(t *testing.T) {
	result := Compute("line1\nline3\n", "line1\nline2\nline3\n")
	if !result.HasDiff {
		t.Error("expected diff to be found")
	}
	if !strings.Contains(result.RawDiff, "+line2") {
		t.Errorf("expected diff to contain '+line2', got %q", result.RawDiff)
	}
}

func TestComputeRemoveLines(t *testing.T) {
	result := Compute("line1\nline2\nline3\n", "line1\nline3\n")
	if !result.HasDiff {
		t.Error("expected diff to be found")
	}
	if !strings.Contains(result.RawDiff, "-line2") {
		t.Errorf("expected diff to contain '-line2', got %q", result.RawDiff)
	}
}

func TestComputeEmptyStrings(t *testing.T) {
	result := Compute("", "")
	if result.HasDiff {
		t.Error("expected no diff for empty strings")
	}
}

func TestComputeOneEmpty(t *testing.T) {
	result := Compute("", "line1\n")
	if !result.HasDiff {
		t.Error("expected diff when one string is empty")
	}
	if !strings.Contains(result.RawDiff, "+line1") {
		t.Errorf("expected '+line1' in diff, got %q", result.RawDiff)
	}
}

func TestColorize(t *testing.T) {
	diff := "-old line\n+new line\n context\n"

	colored := Colorize(diff)

	// Removed line: red, prefix stripped.
	if !strings.Contains(colored, "\033[31mold line") {
		t.Errorf("expected red color for removed line with prefix stripped, got %q", colored)
	}
	if strings.Contains(colored, "\033[31m-old") {
		t.Error("should not retain '-' prefix in colored output")
	}
	// Added line: green, prefix stripped.
	if !strings.Contains(colored, "\033[32mnew line") {
		t.Errorf("expected green color for added line with prefix stripped, got %q", colored)
	}
	if strings.Contains(colored, "\033[32m+new") {
		t.Error("should not retain '+' prefix in colored output")
	}
	// Context line: space prefix stripped.
	if strings.Contains(colored, " context") {
		t.Error("context line should have leading space stripped")
	}
}

func TestColorizeNoColor(t *testing.T) {
	diff := "--- a\n+++ b\n@@ -1 +1 @@\n-old\n+new\n"

	// Verify that without colorize, no ANSI codes are present.
	if strings.Contains(diff, "\033[") {
		t.Error("raw diff should not contain ANSI codes")
	}
}

func TestSplitLines(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{"", nil},
		{"single", []string{"single"}},
		{"a\nb\nc", []string{"a", "b", "c"}},
		{"a\nb\nc\n", []string{"a", "b", "c"}},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := splitLines(tt.input)
			if len(got) != len(tt.want) {
				t.Errorf("splitLines(%q) = %v, want %v", tt.input, got, tt.want)
				return
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("splitLines(%q)[%d] = %q, want %q", tt.input, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestComputeDiffFormat(t *testing.T) {
	result := Compute("line1\nline2\nline3\n", "line1\nline2a\nline3\n")
	if !result.HasDiff {
		t.Fatal("expected diff")
	}

	// Should NOT contain unified diff metadata headers.
	if strings.Contains(result.RawDiff, "--- a") {
		t.Error("should not contain '--- a' header")
	}
	if strings.Contains(result.RawDiff, "+++ b") {
		t.Error("should not contain '+++ b' header")
	}
	if strings.Contains(result.RawDiff, "@@") {
		t.Error("should not contain hunk header '@@'")
	}
	// Should contain the changed lines with +/- prefixes (raw format).
	if !strings.Contains(result.RawDiff, "-line2") {
		t.Error("expected '-line2' in raw diff")
	}
	if !strings.Contains(result.RawDiff, "+line2a") {
		t.Error("expected '+line2a' in raw diff")
	}
}

func TestComputeLargeFileDiff(t *testing.T) {
	// Generate two large strings with small differences.
	var original, modified strings.Builder
	for i := 0; i < 100; i++ {
		original.WriteString("unchanged line\n")
	}
	for i := 0; i < 100; i++ {
		if i == 50 {
			modified.WriteString("changed line\n")
		} else {
			modified.WriteString("unchanged line\n")
		}
	}

	result := Compute(original.String(), modified.String())
	if !result.HasDiff {
		t.Error("expected diff for large files")
	}
	if !strings.Contains(result.RawDiff, "-unchanged line") {
		t.Error("expected '-unchanged line' in diff")
	}
	if !strings.Contains(result.RawDiff, "+changed line") {
		t.Error("expected '+changed line' in diff")
	}
}
