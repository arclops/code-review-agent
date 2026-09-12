package diff

import (
	"fmt"
	"reflect"
	"testing"
)

// multiHunkPatch is a realistic two hunk patch. The numbers below are the
// numbers the lines have in the new file.
const multiHunkPatch = `diff --git a/app.py b/app.py
index 4f2a1c9..8b3d5e1 100644
--- a/app.py
+++ b/app.py
@@ -1,4 +1,5 @@
 import os
-x = 1
+y = 2
+print(y)
 
 def main():
@@ -10,3 +11,4 @@ def main():
     pass
+z = 3
+# comment
+print(z)
`

func TestParseNumbersLinesWithTheirNumberInTheNewFile(t *testing.T) {
	want := []Line{
		{Number: 1, Text: "import os"},
		{Number: 2, Text: "y = 2", Added: true},
		{Number: 3, Text: "print(y)", Added: true},
		{Number: 4, Text: ""},
		{Number: 5, Text: "def main():"},
		{Number: 11, Text: "    pass"},
		{Number: 12, Text: "z = 3", Added: true},
		{Number: 13, Text: "# comment", Added: true},
		{Number: 14, Text: "print(z)", Added: true},
	}

	got := Parse(multiHunkPatch)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Parse() =\n%+v\nwant\n%+v", got, want)
	}
}

func TestParseDoesNotCallContextOrRemovedLinesAdded(t *testing.T) {
	for _, line := range Parse(multiHunkPatch) {
		switch line.Text {
		case "import os", "", "def main():", "    pass":
			if line.Added {
				t.Errorf("line %d (%q) is marked added, want a context line", line.Number, line.Text)
			}
		case "y = 2", "print(y)", "z = 3", "# comment", "print(z)":
			if !line.Added {
				t.Errorf("line %d (%q) is not marked added, want an added line", line.Number, line.Text)
			}
		}
		if line.Text == "x = 1" {
			t.Errorf("line %d is the removed line %q, want removed lines left out entirely", line.Number, line.Text)
		}
	}
}

func TestParseHunkHeaderForms(t *testing.T) {
	cases := []struct {
		name  string
		patch string
		want  []Line
	}{
		{
			name:  "a header with counts",
			patch: "@@ -1,4 +1,5 @@\n context\n+added\n",
			want:  []Line{{Number: 1, Text: "context"}, {Number: 2, Text: "added", Added: true}},
		},
		{
			name:  "a header with no counts at all",
			patch: "@@ -1 +1 @@\n-old\n+new\n",
			want:  []Line{{Number: 1, Text: "new", Added: true}},
		},
		{
			name:  "a header with a count on the old side only",
			patch: "@@ -7 +9 @@\n-x\n+y\n",
			want:  []Line{{Number: 9, Text: "y", Added: true}},
		},
		{
			name:  "a header with a count on the new side only",
			patch: "@@ -1,2 +5,2 @@\n context\n+added\n",
			want:  []Line{{Number: 5, Text: "context"}, {Number: 6, Text: "added", Added: true}},
		},
		{
			name:  "a new file starts at line one",
			patch: "@@ -0,0 +1,2 @@\n+a\n+b\n",
			want:  []Line{{Number: 1, Text: "a", Added: true}, {Number: 2, Text: "b", Added: true}},
		},
		{
			name:  "a removed line does not advance the new numbering",
			patch: "@@ -1,3 +1,3 @@\n a\n-b\n+c\n d\n",
			want: []Line{
				{Number: 1, Text: "a"},
				{Number: 2, Text: "c", Added: true},
				{Number: 3, Text: "d"},
			},
		},
		{
			name:  "a bare plus adds an empty line",
			patch: "@@ -1 +1 @@\n+\n",
			want:  []Line{{Number: 1, Text: "", Added: true}},
		},
		{
			name:  "text after the closing marker is ignored",
			patch: "@@ -1,2 +1,2 @@ func main() {\n+added\n",
			want:  []Line{{Number: 1, Text: "added", Added: true}},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := Parse(testCase.patch)
			if !reflect.DeepEqual(got, testCase.want) {
				t.Errorf("Parse() = %+v, want %+v", got, testCase.want)
			}
		})
	}
}

func TestParseIgnoresTheNoNewlineMarker(t *testing.T) {
	cases := []struct {
		name  string
		patch string
		want  []Line
	}{
		{
			name:  "the marker after an added line",
			patch: "@@ -1,2 +1,2 @@\n-a\n+b\n\\ No newline at end of file\n",
			want:  []Line{{Number: 1, Text: "b", Added: true}},
		},
		{
			name:  "the marker after a context line",
			patch: "@@ -1,2 +1,3 @@\n first\n+second\n\\ No newline at end of file\n",
			want:  []Line{{Number: 1, Text: "first"}, {Number: 2, Text: "second", Added: true}},
		},
		{
			name:  "the marker before the next hunk",
			patch: "@@ -1,1 +1,2 @@\n a\n+b\n\\ No newline at end of file\n@@ -5,1 +6,2 @@\n c\n+d\n",
			want: []Line{
				{Number: 1, Text: "a"},
				{Number: 2, Text: "b", Added: true},
				{Number: 6, Text: "c"},
				{Number: 7, Text: "d", Added: true},
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := Parse(testCase.patch)
			if !reflect.DeepEqual(got, testCase.want) {
				t.Errorf("Parse() = %+v, want %+v", got, testCase.want)
			}
		})
	}
}

func TestParseKeepsLineNumbersIncreasingAcrossHunks(t *testing.T) {
	patch := `diff --git a/main.go b/main.go
--- a/main.go
+++ b/main.go
@@ -2,2 +2,3 @@
 b
+B
 c
@@ -20,1 +21,2 @@
 t
+U
@@ -100,1 +102,2 @@
 x
+Y
`

	lines := Parse(patch)
	if len(lines) == 0 {
		t.Fatal("Parse() returned no lines")
	}
	for index := 1; index < len(lines); index++ {
		if lines[index].Number <= lines[index-1].Number {
			t.Errorf("line %d has number %d after number %d, want the numbering to keep increasing",
				index, lines[index].Number, lines[index-1].Number)
		}
	}

	wantNumbers := []int{2, 3, 4, 21, 22, 102, 103}
	gotNumbers := make([]int, 0, len(lines))
	for _, line := range lines {
		gotNumbers = append(gotNumbers, line.Number)
	}
	if !reflect.DeepEqual(gotNumbers, wantNumbers) {
		t.Errorf("numbers = %v, want %v", gotNumbers, wantNumbers)
	}
}

func TestParseSurvivesEmptyAndMalformedPatches(t *testing.T) {
	cases := []struct {
		name  string
		patch string
	}{
		{name: "empty", patch: ""},
		{name: "only newlines", patch: "\n\n\n"},
		{name: "prose", patch: "this is not a diff at all"},
		{name: "file headers without a hunk", patch: "diff --git a/x.py b/x.py\nindex 1..2 100644\n--- a/x.py\n+++ b/x.py\n"},
		{name: "a malformed hunk header", patch: "@@ -x +y @@\n+line\n"},
		{name: "a hunk header with no new side", patch: "@@ -1 + @@\n+line\n"},
		{name: "lines before any hunk", patch: "+++ b/x.py\n+orphan\n"},
		{name: "a lone marker", patch: "\\ No newline at end of file\n"},
		{name: "a hunk header and nothing else", patch: "@@ -1 +1 @@\n"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			lines := Parse(testCase.patch)
			if len(lines) != 0 {
				t.Errorf("Parse(%q) = %+v, want no lines", testCase.patch, lines)
			}
			if added := AddedLines(testCase.patch); len(added) != 0 {
				t.Errorf("AddedLines(%q) = %+v, want no lines", testCase.patch, added)
			}
			if touched := Touched(testCase.patch); len(touched) != 0 {
				t.Errorf("Touched(%q) = %v, want an empty set", testCase.patch, touched)
			}
		})
	}
}

func TestAddedLinesReturnsOnlyAddedLinesInOrder(t *testing.T) {
	want := []Line{
		{Number: 2, Text: "y = 2", Added: true},
		{Number: 3, Text: "print(y)", Added: true},
		{Number: 12, Text: "z = 3", Added: true},
		{Number: 13, Text: "# comment", Added: true},
		{Number: 14, Text: "print(z)", Added: true},
	}
	got := AddedLines(multiHunkPatch)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AddedLines() = %+v, want %+v", got, want)
	}
}

func TestTouchedIncludesContextAndAddedLinesInTheNewFile(t *testing.T) {
	got := Touched(multiHunkPatch)
	want := map[int]bool{1: true, 2: true, 3: true, 4: true, 5: true, 11: true, 12: true, 13: true, 14: true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Touched() = %v, want %v", got, want)
	}
	if got[0] {
		t.Error("Touched() contains line 0, want hunk-relative numbers only after a header")
	}
}

func TestEmptyPatchHasNoLinesAndZeroStats(t *testing.T) {
	if lines := Parse(""); lines != nil {
		t.Errorf("Parse(\"\") = %+v, want nil", lines)
	}
	if added := AddedLines(""); added != nil {
		t.Errorf("AddedLines(\"\") = %+v, want nil", added)
	}
	if touched := Touched(""); len(touched) != 0 {
		t.Errorf("Touched(\"\") = %v, want an empty map", touched)
	}
	added, removed := Stats("")
	if added != 0 || removed != 0 {
		t.Errorf("Stats(\"\") = %d, %d, want 0, 0", added, removed)
	}
	if summary := Summary(""); summary != "+0/-0" {
		t.Errorf("Summary(\"\") = %q, want %q", summary, "+0/-0")
	}
}

func TestStatsSummaryAndAddedLinesAgree(t *testing.T) {
	cases := []struct {
		name        string
		patch       string
		wantAdded   int
		wantRemoved int
	}{
		{
			name:        "two hunks adding and removing",
			patch:       multiHunkPatch,
			wantAdded:   5,
			wantRemoved: 1,
		},
		{
			name:        "additions only",
			patch:       "diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -1,1 +1,3 @@\n a\n+b\n+c\n",
			wantAdded:   2,
			wantRemoved: 0,
		},
		{
			name:        "deletions only",
			patch:       "@@ -1,3 +1,1 @@\n-a\n-b\n c\n",
			wantAdded:   0,
			wantRemoved: 2,
		},
		{
			name:        "context only",
			patch:       "@@ -1,2 +1,2 @@\n a\n b\n",
			wantAdded:   0,
			wantRemoved: 0,
		},
		{
			name:        "an added empty line counts",
			patch:       "@@ -1,1 +1,2 @@\n a\n+\n",
			wantAdded:   1,
			wantRemoved: 0,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			added, removed := Stats(testCase.patch)
			if added != testCase.wantAdded || removed != testCase.wantRemoved {
				t.Errorf("Stats() = %d, %d, want %d, %d", added, removed, testCase.wantAdded, testCase.wantRemoved)
			}
			if got := len(AddedLines(testCase.patch)); got != testCase.wantAdded {
				t.Errorf("len(AddedLines()) = %d, want %d: AddedLines and Stats must agree", got, testCase.wantAdded)
			}
			wantSummary := fmt.Sprintf("+%d/-%d", testCase.wantAdded, testCase.wantRemoved)
			if got := Summary(testCase.patch); got != wantSummary {
				t.Errorf("Summary() = %q, want %q", got, wantSummary)
			}
		})
	}
}

func TestStatsDoesNotCountTheFileHeaders(t *testing.T) {
	patch := "diff --git a/x.py b/x.py\nindex 12ab..34cd 100644\n--- a/x.py\n+++ b/x.py\n@@ -1 +1 @@\n-old\n+new\n"
	added, removed := Stats(patch)
	if added != 1 {
		t.Errorf("added = %d, want 1: the +++ header is not an added line", added)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1: the --- header is not a removed line", removed)
	}
}

func TestParseKeepsTextVerbatimIncludingLeadingWhitespace(t *testing.T) {
	patch := "@@ -1,2 +1,3 @@\n\tindented\n+    four spaces\n+  trailing  \n"
	want := []Line{
		{Number: 1, Text: "\tindented"},
		{Number: 2, Text: "    four spaces", Added: true},
		{Number: 3, Text: "  trailing  ", Added: true},
	}
	got := Parse(patch)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Parse() = %+v, want %+v", got, want)
	}
}
