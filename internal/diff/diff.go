// Package diff provides diff computation, formatting, and coloring for flux-diff.
package diff

import (
	"fmt"
	"os"
	"strings"
)

// Exit codes for diff operations.
const (
	ExitSuccess      = 0
	ExitDiffFound    = 1
	ExitError        = 2
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
	ansiRed    = "\033[31m"
	ansiGreen  = "\033[32m"
	ansiCyan   = "\033[36m"
	ansiBold   = "\033[1m"
	ansiReset  = "\033[0m"
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

// unifiedDiff computes a simple unified diff between two line slices.
// This is a basic implementation — for production use, a proper diff algorithm
// (Myers, patience, etc.) would be preferred, but this covers the core use case.
func unifiedDiff(original, modified []string) string {
	// Use a simple line-by-line comparison with LCS-based diff.
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

	// Group changes into hunks and format them.
	hunks := groupIntoHunks(ops, original, modified)
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
	op    byte // 'e' equal, 'd' delete, 'i' insert
	line  string
	oIdx  int
	nIdx  int
}

type hunk struct {
	oldStart int
	oldCount int
	newStart int
	newCount int
	lines    []string
}

func groupIntoHunks(ops []editOp, original, modified []string) []hunk {
	if len(ops) == 0 {
		return nil
	}

	var hunks []hunk
	current := hunk{
		oldStart: ops[0].oIdx + 1,
		newStart: ops[0].nIdx + 1,
	}

	if current.oldStart < 1 {
		current.oldStart = 1
	}
	if current.newStart < 1 {
		current.newStart = 1
	}

	// Add context before first change.
	contextLines := 3
	changeStart := 0
	for i, op := range ops {
		if op.op != 'e' {
			changeStart = i
			break
		}
	}

	// Adjust start with leading context.
	contextBefore := min(contextLines, changeStart)
	for i := changeStart - contextBefore; i < changeStart; i++ {
		current.lines = append(current.lines, " "+ops[i].line)
		current.oldCount++
		current.newCount++
	}

	inChange := false
	for i, op := range ops {
		switch op.op {
		case 'e':
			// Check if we need to start a new hunk after a gap.
			if inChange {
				// Add trailing context.
				contextAfter := min(contextLines, len(ops)-i)
				for j := i; j < i+contextAfter && j < len(ops) && ops[j].op == 'e'; j++ {
					current.lines = append(current.lines, " "+ops[j].line)
					current.oldCount++
					current.newCount++
				}
				hunks = append(hunks, current)

				// Start new hunk for remaining changes.
				current = hunk{}
				inChange = false
			}
		case 'd':
			current.lines = append(current.lines, "-"+op.line)
			current.oldCount++
			inChange = true
		case 'i':
			current.lines = append(current.lines, "+"+op.line)
			current.newCount++
			inChange = true
		}

		_ = i // suppress unused warning if needed
	}

	if len(current.lines) > 0 {
		hunks = append(hunks, current)
	}

	return hunks
}

// computeEditScript computes a minimal edit script using the LCS algorithm.
func computeEditScript(original, modified []string) []editOp {
	m, n := len(original), len(modified)

	// Build LCS table.
	dp := make([][]int, m+1)
	for i := range dp {
		dp[i] = make([]int, n+1)
	}

	for i := 1; i <= m; i++ {
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

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
