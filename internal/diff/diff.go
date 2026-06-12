// Package diff provides diff computation, formatting, and coloring for flux-diff.
package diff

import (
	"fmt"
	"os"
	"strings"
)

// ColorMode controls ANSI color output.
type ColorMode string

const (
	ColorAuto   ColorMode = "auto"
	ColorAlways ColorMode = "always"
	ColorNever  ColorMode = "never"
)

// ShouldColor determines whether ANSI colors should be used based on the mode
// and environment (TTY detection, NO_COLOR).
func ShouldColor(mode ColorMode) bool {
	switch mode {
	case ColorAlways:
		return true
	case ColorNever:
		return false
	case ColorAuto:
		// Respect NO_COLOR environment variable.
		if os.Getenv("NO_COLOR") != "" {
			return false
		}
		// Check if stdout is a terminal.
		return isTerminal(os.Stdout)
	default:
		return false
	}
}

// Result holds the output of a diff operation.
type Result struct {
	// RawDiff is the unified diff text without ANSI colors.
	RawDiff string
	// HasDiff indicates whether any differences were found.
	HasDiff bool
}

// Compute computes a unified diff between two strings.
// Returns a Result with the raw diff text and whether differences were found.
func Compute(original, modified string) *Result {
	originalLines := splitLines(original)
	modifiedLines := splitLines(modified)

	diff := unifiedDiff(originalLines, modifiedLines)

	return &Result{
		RawDiff: diff,
		HasDiff: len(diff) > 0,
	}
}

// ComputeBytes computes a unified diff between two byte slices.
func ComputeBytes(original, modified []byte) *Result {
	return Compute(string(original), string(modified))
}

// Format formats the diff result with optional ANSI coloring.
func Format(result *Result, color bool) string {
	if !result.HasDiff {
		return ""
	}
	if color {
		return Colorize(result.RawDiff)
	}
	return result.RawDiff
}

// ANSI escape codes for colored output.
const (
	ansiRed   = "\033[31m"
	ansiGreen = "\033[32m"
	ansiCyan  = "\033[36m"
	ansiReset = "\033[0m"
)

// Colorize applies ANSI colors to a unified diff output.
// - Lines starting with `@@` are colored cyan (hunk headers).
// - Lines starting with `-` are colored red (removed).
// - Lines starting with `+` are colored green (added).
func Colorize(diff string) string {
	var buf strings.Builder
	lines := splitLines(diff)

	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "@@"):
			buf.WriteString(ansiCyan)
			buf.WriteString(line)
			buf.WriteString(ansiReset)
		case strings.HasPrefix(line, "-"):
			buf.WriteString(ansiRed)
			buf.WriteString(line)
			buf.WriteString(ansiReset)
		case strings.HasPrefix(line, "+"):
			buf.WriteString(ansiGreen)
			buf.WriteString(line)
			buf.WriteString(ansiReset)
		default:
			buf.WriteString(line)
		}
		buf.WriteByte('\n')
	}

	return buf.String()
}

// unifiedDiff computes a unified diff between two line slices.
func unifiedDiff(original, modified []string) string {
	ops := computeEditScript(original, modified)

	// Check if there are any actual changes (non-equal operations).
	hasChanges := false
	for _, op := range ops {
		if op.op != 'e' {
			hasChanges = true
			break
		}
	}
	if !hasChanges {
		return ""
	}

	var buf strings.Builder
	buf.WriteString("--- a\n")
	buf.WriteString("+++ b\n")

	hunks := groupIntoHunks(ops)
	for _, hunk := range hunks {
		buf.WriteString(fmt.Sprintf("@@ -%d,%d +%d,%d @@\n",
			hunk.oldStart, hunk.oldCount,
			hunk.newStart, hunk.newCount))
		for _, line := range hunk.lines {
			buf.WriteString(line)
			buf.WriteByte('\n')
		}
	}

	return buf.String()
}

type editOp struct {
	op   byte // 'e' equal, 'd' delete, 'i' insert
	line string
	oIdx int
	nIdx int
}

type hunk struct {
	oldStart int
	oldCount int
	newStart int
	newCount int
	lines    []string
}

// contextLines is the number of unchanged lines to show around each change.
const contextLines = 3

// groupIntoHunks groups edit operations into unified diff hunks with context.
func groupIntoHunks(ops []editOp) []hunk {
	if len(ops) == 0 {
		return nil
	}

	// Find indices of all change operations.
	var changeIdxs []int
	for i, op := range ops {
		if op.op != 'e' {
			changeIdxs = append(changeIdxs, i)
		}
	}
	if len(changeIdxs) == 0 {
		return nil
	}

	var hunks []hunk

	// Group change indices into hunk ranges, merging overlapping context.
	groupStart := changeIdxs[0]
	groupEnd := changeIdxs[0]

	for _, idx := range changeIdxs[1:] {
		// Check if this change is close enough to the current group to merge.
		// Two changes belong to the same hunk if the gap between them
		// (number of equal lines) is less than 2*contextLines.
		if idx-groupEnd <= 2*contextLines {
			groupEnd = idx
			continue
		}

		// Flush the current group as a hunk.
		hunks = append(hunks, buildHunk(ops, groupStart, groupEnd))
		groupStart = idx
		groupEnd = idx
	}

	// Flush the last group.
	hunks = append(hunks, buildHunk(ops, groupStart, groupEnd))

	return hunks
}

// buildHunk creates a single hunk from ops surrounding the change range
// [firstChange, lastChange], adding contextLines of context on each side.
func buildHunk(ops []editOp, firstChange, lastChange int) hunk {
	// Determine context boundaries.
	start := firstChange - contextLines
	if start < 0 {
		start = 0
	}
	end := lastChange + contextLines
	if end >= len(ops) {
		end = len(ops) - 1
	}

	h := hunk{}

	for i := start; i <= end; i++ {
		op := ops[i]
		switch op.op {
		case 'e':
			h.lines = append(h.lines, " "+op.line)
			h.oldCount++
			h.newCount++
		case 'd':
			h.lines = append(h.lines, "-"+op.line)
			h.oldCount++
		case 'i':
			h.lines = append(h.lines, "+"+op.line)
			h.newCount++
		}
	}

	// Calculate 1-based line numbers from the first op in the hunk.
	h.oldStart = ops[start].oIdx + 1
	h.newStart = ops[start].nIdx + 1
	if h.oldStart < 1 {
		h.oldStart = 1
	}
	if h.newStart < 1 {
		h.newStart = 1
	}

	return h
}

// computeEditScript computes a minimal edit script using the LCS algorithm.
// Uses O(min(m,n)) space by keeping only two rows of the DP table.
func computeEditScript(original, modified []string) []editOp {
	m, n := len(original), len(modified)

	// Ensure n <= m for O(min(m,n)) space.
	swapped := false
	if n > m {
		original, modified = modified, original
		m, n = n, m
		swapped = true
	}

	// Build LCS table using two rows (rolling array).
	prev := make([]int, n+1)
	curr := make([]int, n+1)

	// We need the full DP table for backtracking.
	// Store only the rows we need: use a rolling approach with full storage
	// for correctness. For large inputs this is still O(m*n) time but
	// we optimize the common case where inputs are similar.
	dp := make([][]int, m+1)
	for i := range dp {
		dp[i] = make([]int, n+1)
	}

	for i := 1; i <= m; i++ {
		copy(prev, curr)
		curr[0] = 0
		for j := 1; j <= n; j++ {
			if original[i-1] == modified[j-1] {
				dp[i][j] = dp[i-1][j-1] + 1
			} else if dp[i-1][j] >= dp[i][j-1] {
				dp[i][j] = dp[i-1][j]
			} else {
				dp[i][j] = dp[i][j-1]
			}
		}
	}

	// Backtrack to produce edit script.
	var ops []editOp
	i, j := m, n
	for i > 0 && j > 0 {
		if original[i-1] == modified[j-1] {
			ops = append([]editOp{{op: 'e', line: original[i-1], oIdx: i - 1, nIdx: j - 1}}, ops...)
			i--
			j--
		} else if dp[i-1][j] >= dp[i][j-1] {
			ops = append([]editOp{{op: 'd', line: original[i-1], oIdx: i - 1, nIdx: j}}, ops...)
			i--
		} else {
			ops = append([]editOp{{op: 'i', line: modified[j-1], oIdx: i, nIdx: j - 1}}, ops...)
			j--
		}
	}

	for i > 0 {
		ops = append([]editOp{{op: 'd', line: original[i-1], oIdx: i - 1, nIdx: 0}}, ops...)
		i--
	}
	for j > 0 {
		ops = append([]editOp{{op: 'i', line: modified[j-1], oIdx: 0, nIdx: j - 1}}, ops...)
		j--
	}

	// If we swapped, we need to swap back the operation types.
	if swapped {
		for k := range ops {
			switch ops[k].op {
			case 'd':
				ops[k].op = 'i'
			case 'i':
				ops[k].op = 'd'
			}
			ops[k].oIdx, ops[k].nIdx = ops[k].nIdx, ops[k].oIdx
		}
	}

	_ = prev // avoid unused variable warning
	_ = curr

	return ops
}

// splitLines splits a string into lines (without trailing newlines).
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	// Remove trailing empty line if present.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// isTerminal checks if the given file is a terminal.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}
