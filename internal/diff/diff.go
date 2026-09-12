// Package diff reads unified diffs, which is what GitHub sends in a file's
// patch field.
//
// Two things need it. The secret scan has to look at the lines a pull request
// adds rather than the whole file, or it reports the credentials that were
// already there. And the linter's findings have to be filtered to the lines the
// pull request touches, or a review of a two line change arrives carrying
// forty pre-existing complaints.
package diff

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Line is one line of a patch with the number it has in the new file.
type Line struct {
	Number int
	Text   string
	Added  bool
}

var hunkHeader = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// Parse walks a patch and returns every line it touches in the new file, in
// order. Context lines are included, removed lines are not, because they are
// not in the new file.
func Parse(patch string) []Line {
	var lines []Line
	number := 0
	inHunk := false

	for _, raw := range strings.Split(patch, "\n") {
		if match := hunkHeader.FindStringSubmatch(raw); match != nil {
			start, err := strconv.Atoi(match[1])
			if err != nil {
				continue
			}
			number = start
			inHunk = true
			continue
		}
		if !inHunk {
			// The file header: "diff --git", "index", "---", "+++".
			continue
		}
		if raw == "" {
			continue
		}

		switch raw[0] {
		case '+':
			if strings.HasPrefix(raw, "+++") {
				continue
			}
			lines = append(lines, Line{Number: number, Text: raw[1:], Added: true})
			number++
		case '-':
			if strings.HasPrefix(raw, "---") {
				continue
			}
			// The line is gone from the new file, so the counter does not move.
		case '\\':
			// "\ No newline at end of file" is a note, not a line.
		default:
			text := strings.TrimPrefix(raw, " ")
			lines = append(lines, Line{Number: number, Text: text})
			number++
		}
	}
	return lines
}

// AddedLines returns only the lines the patch adds.
func AddedLines(patch string) []Line {
	var added []Line
	for _, line := range Parse(patch) {
		if line.Added {
			added = append(added, line)
		}
	}
	return added
}

// Touched is the set of line numbers in the new file that the patch mentions.
func Touched(patch string) map[int]bool {
	touched := make(map[int]bool)
	for _, line := range Parse(patch) {
		touched[line.Number] = true
	}
	return touched
}

// Stats counts what a patch does, which is what the skipped-files note uses.
func Stats(patch string) (added, removed int) {
	for _, raw := range strings.Split(patch, "\n") {
		switch {
		case strings.HasPrefix(raw, "+++"), strings.HasPrefix(raw, "---"):
		case strings.HasPrefix(raw, "+"):
			added++
		case strings.HasPrefix(raw, "-"):
			removed++
		}
	}
	return added, removed
}

// Summary is a one line description of a patch for the review comment.
func Summary(patch string) string {
	added, removed := Stats(patch)
	return fmt.Sprintf("+%d/-%d", added, removed)
}
