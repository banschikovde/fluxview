// Package diff provides diff computation, formatting, and coloring for fluxview.
package diff

import (
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

// DefaultContext is the default number of context lines around changes.
const DefaultContext = 3

// Compute computes a diff between two strings with default context lines.
func Compute(original, modified string) *Result {
	return ComputeCtx(original, modified, DefaultContext)
}

// ComputeCtx computes a diff with the specified number of context lines.
func ComputeCtx(original, modified string, ctxLines int) *Result {
	if ctxLines < 0 {
		ctxLines = DefaultContext
	}
	originalLines := splitLines(original)
	modifiedLines := splitLines(modified)

	diff := unifiedDiff(originalLines, modifiedLines, ctxLines)

	return &Result{
		RawDiff: diff,
		HasDiff: len(diff) > 0,
	}
}

// ANSI escape codes for colored output.
const (
	ANSIRed   = "\033[31m"
	ANSIGreen = "\033[32m"
	ANSIReset = "\033[0m"
)

// Colorize applies ANSI colors to a diff output, stripping the +/-/space
// prefixes so only color distinguishes changes from context.
// - Lines starting with `-` are colored red (removed), prefix stripped.
// - Lines starting with `+` are colored green (added), prefix stripped.
// - Context lines (leading space) have the prefix stripped.
func Colorize(diff string) string {
	var buf strings.Builder
	lines := splitLines(diff)

	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "-"):
			buf.WriteString(ANSIRed)
			buf.WriteString(line[1:])
			buf.WriteString(ANSIReset)
		case strings.HasPrefix(line, "+"):
			buf.WriteString(ANSIGreen)
			buf.WriteString(line[1:])
			buf.WriteString(ANSIReset)
		default:
			if len(line) > 0 && line[0] == ' ' {
				buf.WriteString(line[1:])
			} else {
				buf.WriteString(line)
			}
		}
		buf.WriteByte('\n')
	}

	return buf.String()
}

// unifiedDiff computes a unified diff between two line slices.
func unifiedDiff(original, modified []string, ctxLines int) string {
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

	hunks := groupIntoHunks(ops, ctxLines)
	for _, hunk := range hunks {
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
}

type hunk struct {
	lines []string
}

// groupIntoHunks groups edit operations into diff hunks with context.
func groupIntoHunks(ops []editOp, ctxLines int) []hunk {
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
		// (number of equal lines) is less than 2*ctxLines.
		if idx-groupEnd <= 2*ctxLines {
			groupEnd = idx
			continue
		}

		// Flush the current group as a hunk.
		hunks = append(hunks, buildHunk(ops, groupStart, groupEnd, ctxLines))
		groupStart = idx
		groupEnd = idx
	}

	// Flush the last group.
	hunks = append(hunks, buildHunk(ops, groupStart, groupEnd, ctxLines))

	return hunks
}

// buildHunk creates a single hunk from ops surrounding the change range
// [firstChange, lastChange], adding ctxLines of context on each side.
func buildHunk(ops []editOp, firstChange, lastChange, ctxLines int) hunk {
	// Determine context boundaries.
	start := firstChange - ctxLines
	if start < 0 {
		start = 0
	}
	end := lastChange + ctxLines
	if end >= len(ops) {
		end = len(ops) - 1
	}

	h := hunk{}

	for i := start; i <= end; i++ {
		op := ops[i]
		switch op.op {
		case 'e':
			h.lines = append(h.lines, " "+op.line)
		case 'd':
			h.lines = append(h.lines, "-"+op.line)
		case 'i':
			h.lines = append(h.lines, "+"+op.line)
		}
	}

	return h
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

	// Backtrack to produce edit script (append in reverse, then reverse).
	var ops []editOp
	i, j := m, n
	for i > 0 && j > 0 {
		if original[i-1] == modified[j-1] {
			ops = append(ops, editOp{op: 'e', line: original[i-1]})
			i--
			j--
		} else if dp[i-1][j] >= dp[i][j-1] {
			ops = append(ops, editOp{op: 'd', line: original[i-1]})
			i--
		} else {
			ops = append(ops, editOp{op: 'i', line: modified[j-1]})
			j--
		}
	}

	for i > 0 {
		ops = append(ops, editOp{op: 'd', line: original[i-1]})
		i--
	}
	for j > 0 {
		ops = append(ops, editOp{op: 'i', line: modified[j-1]})
		j--
	}

	// Reverse to get correct order.
	for l, r := 0, len(ops)-1; l < r; l, r = l+1, r-1 {
		ops[l], ops[r] = ops[r], ops[l]
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
