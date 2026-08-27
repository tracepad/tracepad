package server

import (
	"fmt"
	"strings"
)

// Unified text diff (spec 004 #21). Structural JSON diffing would need a diff
// vocabulary of our own; every consumer — human, agent, `patch` — already
// reads unified patches, so the version diff is one.

// diffContext is how many unchanged lines surround a change, the same three
// every other unified diff uses.
const diffContext = 3

// maxDiffCells bounds the longest-common-subsequence table. Beyond it the two
// versions are reported as a wholesale replacement rather than aligned: a
// prompt that large is not something a line alignment would make readable
// anyway, and the alternative is a gigabyte of table.
const maxDiffCells = 4_000_000

// unifiedDiff renders the change from `before` to `after` under one label,
// returning "" when they are identical.
func unifiedDiff(label, beforeName, afterName, before, after string) string {
	if before == after {
		return ""
	}
	from, to := splitLines(before), splitLines(after)
	hunks := diffHunks(from, to)
	if len(hunks) == 0 {
		return ""
	}
	var out strings.Builder
	fmt.Fprintf(&out, "--- %s (%s)\n", label, beforeName)
	fmt.Fprintf(&out, "+++ %s (%s)\n", label, afterName)
	for _, hunk := range hunks {
		out.WriteString(hunk)
	}
	return out.String()
}

// splitLines splits text into lines without a trailing empty element, so that
// a body with and without a final newline diff the same way.
func splitLines(text string) []string {
	if text == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n")
}

// edit is one line of the aligned result.
type edit struct {
	// op is ' ' for context, '-' for a removal, '+' for an addition.
	op   byte
	line string
}

// diffHunks aligns the two sides and renders the changed regions with
// context.
func diffHunks(from, to []string) []string {
	edits := align(from, to)

	var (
		hunks    []string
		fromLine = 1
		toLine   = 1
	)
	for i := 0; i < len(edits); {
		if edits[i].op == ' ' {
			fromLine++
			toLine++
			i++
			continue
		}
		// A change: back up over the leading context, then run forward
		// until diffContext unchanged lines in a row end the hunk.
		start := i
		leading := 0
		for start > 0 && edits[start-1].op == ' ' && leading < diffContext {
			start--
			leading++
		}
		end := i
		quiet := 0
		for end < len(edits) && quiet <= diffContext {
			if edits[end].op == ' ' {
				quiet++
			} else {
				quiet = 0
			}
			end++
		}
		// Drop the unchanged tail beyond the context window.
		for end > i && quiet > diffContext {
			end--
			quiet--
		}

		hunks = append(hunks, renderHunk(edits[start:end], fromLine-leading, toLine-leading))
		for ; i < end; i++ {
			switch edits[i].op {
			case ' ':
				fromLine++
				toLine++
			case '-':
				fromLine++
			case '+':
				toLine++
			}
		}
	}
	return hunks
}

func renderHunk(edits []edit, fromStart, toStart int) string {
	var fromCount, toCount int
	for _, e := range edits {
		if e.op != '+' {
			fromCount++
		}
		if e.op != '-' {
			toCount++
		}
	}
	var out strings.Builder
	fmt.Fprintf(&out, "@@ -%s +%s @@\n", span(fromStart, fromCount), span(toStart, toCount))
	for _, e := range edits {
		out.WriteByte(e.op)
		out.WriteString(e.line)
		out.WriteByte('\n')
	}
	return out.String()
}

// span renders a hunk range. An empty side starts at the line before it, which
// is what unified diff means by a zero-length range.
func span(start, count int) string {
	if count == 0 {
		return fmt.Sprintf("%d,0", start-1)
	}
	if count == 1 {
		return fmt.Sprintf("%d", start)
	}
	return fmt.Sprintf("%d,%d", start, count)
}

// align pairs the two sides line by line. Common prefixes and suffixes are
// matched directly — they are the bulk of any real version bump — and only
// what is left goes through the longest-common-subsequence table.
func align(from, to []string) []edit {
	var edits []edit

	prefix := 0
	for prefix < len(from) && prefix < len(to) && from[prefix] == to[prefix] {
		edits = append(edits, edit{' ', from[prefix]})
		prefix++
	}
	suffix := 0
	for suffix < len(from)-prefix && suffix < len(to)-prefix &&
		from[len(from)-1-suffix] == to[len(to)-1-suffix] {
		suffix++
	}

	middleFrom, middleTo := from[prefix:len(from)-suffix], to[prefix:len(to)-suffix]
	edits = append(edits, alignMiddle(middleFrom, middleTo)...)
	for i := len(from) - suffix; i < len(from); i++ {
		edits = append(edits, edit{' ', from[i]})
	}
	return edits
}

func alignMiddle(from, to []string) []edit {
	switch {
	case len(from) == 0 && len(to) == 0:
		return nil
	case len(from)*len(to) > maxDiffCells:
		return replaceAll(from, to)
	}

	// lcs[i][j] is the length of the longest common subsequence of
	// from[i:] and to[j:].
	lcs := make([][]int32, len(from)+1)
	for i := range lcs {
		lcs[i] = make([]int32, len(to)+1)
	}
	for i := len(from) - 1; i >= 0; i-- {
		for j := len(to) - 1; j >= 0; j-- {
			if from[i] == to[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}

	var edits []edit
	i, j := 0, 0
	for i < len(from) && j < len(to) {
		switch {
		case from[i] == to[j]:
			edits = append(edits, edit{' ', from[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			edits = append(edits, edit{'-', from[i]})
			i++
		default:
			edits = append(edits, edit{'+', to[j]})
			j++
		}
	}
	return append(edits, replaceAll(from[i:], to[j:])...)
}

// replaceAll renders one side removed and the other added, with no attempt to
// align them.
func replaceAll(from, to []string) []edit {
	edits := make([]edit, 0, len(from)+len(to))
	for _, line := range from {
		edits = append(edits, edit{'-', line})
	}
	for _, line := range to {
		edits = append(edits, edit{'+', line})
	}
	return edits
}
