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

	// Removed line: red, prefix kept.
	if !strings.Contains(colored, "\033[31m-old line") {
		t.Errorf("expected red color for removed line with prefix, got %q", colored)
	}
	// Added line: green, prefix kept.
	if !strings.Contains(colored, "\033[32m+new line") {
		t.Errorf("expected green color for added line with prefix, got %q", colored)
	}
	// Context line: no color, prefix kept.
	if !strings.Contains(colored, " context") {
		t.Error("expected context line to be preserved")
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

func TestMyers_DuplicateLines(t *testing.T) {
	// Multiple duplicate lines — common source of diff bugs.
	result := Compute("a\nb\na\nb\nc\n", "a\nb\na\nb\nX\n")
	if !result.HasDiff {
		t.Fatal("expected diff")
	}
	if !strings.Contains(result.RawDiff, "+X") {
		t.Error("expected +X in diff")
	}
	if !strings.Contains(result.RawDiff, "-c") {
		t.Error("expected -c in diff")
	}
}

func TestMyers_AllDifferent(t *testing.T) {
	result := Compute("a\nb\nc\n", "x\ny\nz\n")
	if !result.HasDiff {
		t.Fatal("expected diff")
	}
	for _, line := range []string{"-a", "-b", "-c", "+x", "+y", "+z"} {
		if !strings.Contains(result.RawDiff, line) {
			t.Errorf("expected %q in diff", line)
		}
	}
}

func TestMyers_InsertInMiddle(t *testing.T) {
	result := Compute("line1\nline2\nline3\n", "line1\nINSERTED\nline2\nline3\n")
	if !result.HasDiff {
		t.Fatal("expected diff")
	}
	if !strings.Contains(result.RawDiff, "+INSERTED") {
		t.Error("expected +INSERTED")
	}
}

func TestMyers_MultipleNonAdjacent(t *testing.T) {
	result := Compute("a\nb\nc\nd\ne\n", "X\nb\nc\nY\ne\n")
	if !result.HasDiff {
		t.Fatal("expected diff")
	}
	if !strings.Contains(result.RawDiff, "+X") || !strings.Contains(result.RawDiff, "+Y") {
		t.Error("expected both +X and +Y")
	}
}

// TestGreedyDiff_LargeInput exercises the greedyDiff fallback path that
// triggers when total input exceeds maxMyersInputSize (50000 lines).
// Verifies correct prefix/suffix matching and middle-block diff output.
func TestGreedyDiff_LargeInput(t *testing.T) {
	// Generate >50000 total lines: common prefix, diff block, common suffix.
	var origLines, modLines []string

	// 20000 lines common prefix.
	for i := 0; i < 20000; i++ {
		origLines = append(origLines, "prefix-"+itos(i))
		modLines = append(modLines, "prefix-"+itos(i))
	}

	// 5000 lines that differ.
	for i := 0; i < 5000; i++ {
		origLines = append(origLines, "old-"+itos(i))
		modLines = append(modLines, "new-"+itos(i))
	}

	// 20000 lines common suffix.
	for i := 0; i < 20000; i++ {
		origLines = append(origLines, "suffix-"+itos(i))
		modLines = append(modLines, "suffix-"+itos(i))
	}

	original := strings.Join(origLines, "\n")
	modified := strings.Join(modLines, "\n")

	result := Compute(original, modified)
	if !result.HasDiff {
		t.Fatal("expected diff for large input")
	}

	// Middle block should produce -old and +new lines.
	if !strings.Contains(result.RawDiff, "-old-0") {
		t.Error("expected '-old-0' in diff (deleted prefix line)")
	}
	if !strings.Contains(result.RawDiff, "+new-0") {
		t.Error("expected '+new-0' in diff (inserted prefix line)")
	}

	// Common prefix and suffix should NOT appear as changes.
	if strings.Contains(result.RawDiff, "-prefix-0") {
		t.Error("common prefix line should not appear as deletion")
	}
	if strings.Contains(result.RawDiff, "+suffix-0") {
		t.Error("common suffix line should not appear as insertion")
	}
}

// itos is a minimal int-to-string helper to avoid strconv import in test.
func itos(n int) string {
	if n == 0 {
		return "0"
	}
	var buf []byte
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	if neg {
		buf = append([]byte{'-'}, buf...)
	}
	return string(buf)
}
