package lint

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/arclops/code-review-agent/internal/config"
)

// testSeverity is the rule to severity mapping a caller would pass in. It is
// deliberately not the package default, so that a parser that ignored it or
// applied it to the wrong rule would be caught.
func testSeverity(rule string) string {
	switch rule {
	case "E501", "no-unused-vars", "vet-printf":
		return "MAJOR"
	default:
		return "MINOR"
	}
}

func TestParsersTurnToolOutputIntoFindings(t *testing.T) {
	tests := []struct {
		name   string
		format string
		output string
		want   []Finding
	}{
		{
			name:   "ruff-json",
			format: "ruff-json",
			output: `[
				{
					"cell": null,
					"code": "F401",
					"end_location": {"column": 10, "row": 1},
					"filename": "/tmp/review/app/main.py",
					"fix": {"applicability": "safe", "edits": [], "message": "Remove unused import: ` + "`os`" + `"},
					"location": {"column": 8, "row": 1},
					"message": "` + "`os`" + ` imported but unused",
					"noqa_row": 1,
					"url": "https://docs.astral.sh/ruff/rules/unused-import"
				},
				{
					"cell": null,
					"code": "E501",
					"end_location": {"column": 96, "row": 12},
					"filename": "/tmp/review/app/main.py",
					"fix": null,
					"location": {"column": 89, "row": 12},
					"message": "Line too long (95 > 88)",
					"noqa_row": 12,
					"url": "https://docs.astral.sh/ruff/rules/line-too-long"
				}
			]`,
			want: []Finding{
				{File: "/tmp/review/app/main.py", Line: 1, Column: 8, Rule: "F401", Message: "`os` imported but unused", Severity: "MINOR"},
				{File: "/tmp/review/app/main.py", Line: 12, Column: 89, Rule: "E501", Message: "Line too long (95 > 88)", Severity: "MAJOR"},
			},
		},
		{
			name:   "ruff-json with no findings",
			format: "ruff-json",
			output: "[]",
			want:   []Finding{},
		},
		{
			name:   "eslint-json",
			format: "eslint-json",
			output: `[
				{
					"filePath": "/tmp/review/web/src/index.js",
					"messages": [
						{
							"ruleId": "no-unused-vars",
							"severity": 2,
							"message": "'payload' is defined but never used.",
							"line": 14,
							"column": 7,
							"nodeType": "Identifier",
							"messageId": "unusedVar",
							"endLine": 14,
							"endColumn": 14
						},
						{
							"ruleId": null,
							"severity": 2,
							"message": "Parsing error: Unexpected token }",
							"line": 22,
							"column": 1,
							"nodeType": null,
							"messageId": "",
							"endLine": 22,
							"endColumn": 1
						}
					],
					"suppressedMessages": [],
					"errorCount": 2,
					"fatalErrorCount": 1,
					"warningCount": 0,
					"fixableErrorCount": 0,
					"fixableWarningCount": 0
				},
				{
					"filePath": "/tmp/review/web/src/clean.js",
					"messages": [],
					"errorCount": 0,
					"warningCount": 0
				}
			]`,
			want: []Finding{
				{File: "/tmp/review/web/src/index.js", Line: 14, Column: 7, Rule: "no-unused-vars", Message: "'payload' is defined but never used.", Severity: "MAJOR"},
				{File: "/tmp/review/web/src/index.js", Line: 22, Column: 1, Rule: "parse-error", Message: "Parsing error: Unexpected token }", Severity: "MINOR"},
			},
		},
		{
			name:   "go-vet",
			format: "go-vet",
			output: "# github.com/arclops/code-review-agent/internal/app\n" +
				"internal/app/main.go:13:3: unreachable code\n" +
				"internal/app/main.go:15:14: fmt.Printf format %d has arg s of wrong type string\n" +
				"internal/app/main.go:29:17: CopyLock passes lock by value: app.L contains sync.Mutex\n" +
				"\n" +
				"vet: exit status 1\n",
			want: []Finding{
				{File: "internal/app/main.go", Line: 13, Column: 3, Rule: "vet-unreachable", Message: "unreachable code", Severity: "MINOR"},
				{File: "internal/app/main.go", Line: 15, Column: 14, Rule: "vet-printf", Message: "fmt.Printf format %d has arg s of wrong type string", Severity: "MAJOR"},
				{File: "internal/app/main.go", Line: 29, Column: 17, Rule: "vet", Message: "CopyLock passes lock by value: app.L contains sync.Mutex", Severity: "MINOR"},
			},
		},
		{
			name:   "go-vet without a column",
			format: "go-vet",
			output: "internal/app/store.go:42: possible misuse of unsafe.Pointer\n",
			want: []Finding{
				{File: "internal/app/store.go", Line: 42, Column: 0, Rule: "vet-misuse", Message: "possible misuse of unsafe.Pointer", Severity: "MINOR"},
			},
		},
		{
			name:   "gofmt-list",
			format: "gofmt-list",
			output: "internal/app/main.go\ninternal/config/config.go\n\n",
			want: []Finding{
				{File: "internal/app/main.go", Rule: "gofmt", Message: "this file is not gofmt'd", Severity: "MINOR"},
				{File: "internal/config/config.go", Rule: "gofmt", Message: "this file is not gofmt'd", Severity: "MINOR"},
			},
		},
		{
			name:   "plain",
			format: "plain",
			output: "shellcheck: SC2086: Double quote to prevent globbing and word splitting.\n\nshellcheck: SC2155: Declare and assign separately.\n",
			want: []Finding{
				{Rule: "output", Message: "shellcheck: SC2086: Double quote to prevent globbing and word splitting."},
				{Rule: "output", Message: "shellcheck: SC2155: Declare and assign separately."},
			},
		},
	}

	covered := make(map[string]bool)
	for _, test := range tests {
		covered[test.format] = true
		t.Run(test.name, func(t *testing.T) {
			parser, ok := Parsers[test.format]
			if !ok {
				t.Fatalf("no parser is registered for %q", test.format)
			}
			got, err := parser([]byte(test.output), testSeverity)
			if err != nil {
				t.Fatalf("parser returned an unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Errorf("findings = %#v, want %#v", got, test.want)
			}
		})
	}

	for format := range Parsers {
		if !covered[format] {
			t.Errorf("parser %q has no sample output in this table", format)
		}
	}
}

func TestParsersReturnNoFindingsInsteadOfPanickingOnMalformedOutput(t *testing.T) {
	tests := []struct {
		name          string
		format        string
		output        string
		wantErrSubstr string
	}{
		{name: "empty ruff output", format: "ruff-json", output: "", wantErrSubstr: "not ruff JSON"},
		{name: "a ruff traceback", format: "ruff-json", output: "Traceback (most recent call last):\n  File \"ruff.py\"\n", wantErrSubstr: "not ruff JSON"},
		{name: "a ruff object instead of a list", format: "ruff-json", output: `{"code":"F401"}`, wantErrSubstr: "not ruff JSON"},
		{name: "truncated ruff JSON", format: "ruff-json", output: `[{"code":"F401",`, wantErrSubstr: "not ruff JSON"},
		{name: "empty eslint output", format: "eslint-json", output: "", wantErrSubstr: "not eslint JSON"},
		{name: "an eslint banner", format: "eslint-json", output: "<html><body>502 Bad Gateway</body></html>", wantErrSubstr: "not eslint JSON"},
		{name: "empty vet output", format: "go-vet", output: ""},
		{name: "vet noise", format: "go-vet", output: "vet: something went wrong\n"},
		{name: "empty gofmt output", format: "gofmt-list", output: ""},
		{name: "whitespace only gofmt output", format: "gofmt-list", output: "\n   \n"},
		{name: "empty plain output", format: "plain", output: ""},
		{name: "whitespace only plain output", format: "plain", output: "\n\t\n"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parser, ok := Parsers[test.format]
			if !ok {
				t.Fatalf("no parser is registered for %q", test.format)
			}
			findings, err := parser([]byte(test.output), testSeverity)
			if len(findings) != 0 {
				t.Errorf("findings = %#v, want none", findings)
			}
			if test.wantErrSubstr == "" {
				if err != nil {
					t.Errorf("error = %v, want none", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want an error mentioning %q, got nil", test.wantErrSubstr)
			}
			if !strings.Contains(err.Error(), test.wantErrSubstr) {
				t.Errorf("error = %v, want it to mention %q", err, test.wantErrSubstr)
			}
		})
	}
}

func TestExecuteKeepsTheFindingsOfALinterThatExitedNonZero(t *testing.T) {
	const output = `[{"code":"F401","filename":"app/main.py","message":"` + "`os`" + ` imported but unused","location":{"row":1,"column":8}}]`

	var gotDir string
	var gotArgs []string
	command := Command{
		Name:     "ruff",
		Args:     []string{"check", "--output-format", "json", "."},
		Format:   "ruff-json",
		Severity: testSeverity,
		Run: func(_ context.Context, dir string, args []string) (string, string, int, error) {
			gotDir, gotArgs = dir, args
			// A linter exits non-zero when it finds something.
			return output, "", 1, nil
		},
	}

	findings, err := command.Execute(context.Background(), "/tmp/review")
	if err != nil {
		t.Fatalf("Execute returned an unexpected error for exit code 1: %v", err)
	}
	want := []Finding{{
		File:     "app/main.py",
		Line:     1,
		Column:   8,
		Rule:     "F401",
		Message:  "`os` imported but unused",
		Severity: "MINOR",
		Tool:     "ruff",
	}}
	if !reflect.DeepEqual(findings, want) {
		t.Errorf("findings = %#v, want %#v", findings, want)
	}
	if gotDir != "/tmp/review" {
		t.Errorf("the tool ran in %q, want %q", gotDir, "/tmp/review")
	}
	if !reflect.DeepEqual(gotArgs, []string{"check", "--output-format", "json", "."}) {
		t.Errorf("the tool ran with %q, want the configured arguments", gotArgs)
	}
}

func TestExecuteDoesNotSwallowACommandThatCouldNotStart(t *testing.T) {
	t.Run("the injected runner failed", func(t *testing.T) {
		command := Command{
			Name:   "ruff",
			Format: "plain",
			Run: func(context.Context, string, []string) (string, string, int, error) {
				return "", "", 0, &exec.Error{Name: "ruff", Err: exec.ErrNotFound}
			},
		}

		findings, err := command.Execute(context.Background(), t.TempDir())
		if err == nil {
			t.Fatal("want an error when the tool could not be started, got nil")
		}
		if len(findings) != 0 {
			t.Errorf("findings = %#v, want none", findings)
		}
		for _, want := range []string{"ruff: could not run", "executable file not found"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %v, want it to mention %q", err, want)
			}
		}
	})

	t.Run("the real runner cannot find the binary", func(t *testing.T) {
		command := Command{
			Name:   "ruff",
			Args:   []string{"code-review-agent-no-such-linter", "--version"},
			Format: "plain",
		}

		_, err := command.Execute(context.Background(), t.TempDir())
		if err == nil {
			t.Fatal("want an error for a binary that is not installed, got nil")
		}
		for _, want := range []string{"ruff: could not run", "code-review-agent-no-such-linter"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %v, want it to mention %q", err, want)
			}
		}
	})
}

func TestExecuteReportsAToolThatFailedWithoutUsableOutput(t *testing.T) {
	command := Command{
		Name:   "ruff",
		Format: "ruff-json",
		Run: func(context.Context, string, []string) (string, string, int, error) {
			return "", "ruff failed to parse pyproject.toml\n", 2, nil
		},
	}

	findings, err := command.Execute(context.Background(), t.TempDir())
	if err == nil {
		t.Fatal("want an error when the tool failed and printed nothing usable, got nil")
	}
	if len(findings) != 0 {
		t.Errorf("findings = %#v, want none", findings)
	}
	for _, want := range []string{"ruff: exited 2", "ruff failed to parse pyproject.toml"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to mention %q", err, want)
		}
	}
}

func TestExecuteReadsTheStreamTheToolWritesItsFindingsTo(t *testing.T) {
	const onStdout = "app/main.go:10:2: unreachable code\n"
	const onStderr = "app/main.go:20:4: fmt.Printf format %d has arg s of wrong type string\n"

	tests := []struct {
		name   string
		stream string
		want   []Finding
	}{
		{
			name:   "stdout when no stream is configured",
			stream: "",
			want: []Finding{
				{File: "app/main.go", Line: 10, Column: 2, Rule: "vet-unreachable", Message: "unreachable code", Severity: "MINOR", Tool: "vet"},
			},
		},
		{
			name:   "stdout when the config names it",
			stream: config.StreamStdout,
			want: []Finding{
				{File: "app/main.go", Line: 10, Column: 2, Rule: "vet-unreachable", Message: "unreachable code", Severity: "MINOR", Tool: "vet"},
			},
		},
		{
			// The Go toolchain reports diagnostics on stderr.
			name:   "stderr",
			stream: config.StreamStderr,
			want: []Finding{
				{File: "app/main.go", Line: 20, Column: 4, Rule: "vet-printf", Message: "fmt.Printf format %d has arg s of wrong type string", Severity: "MINOR", Tool: "vet"},
			},
		},
		{
			name:   "both",
			stream: config.StreamBoth,
			want: []Finding{
				{File: "app/main.go", Line: 10, Column: 2, Rule: "vet-unreachable", Message: "unreachable code", Severity: "MINOR", Tool: "vet"},
				{File: "app/main.go", Line: 20, Column: 4, Rule: "vet-printf", Message: "fmt.Printf format %d has arg s of wrong type string", Severity: "MINOR", Tool: "vet"},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			command := Command{
				Name:   "vet",
				Format: "go-vet",
				Stream: test.stream,
				Run: func(context.Context, string, []string) (string, string, int, error) {
					return onStdout, onStderr, 1, nil
				},
			}

			findings, err := command.Execute(context.Background(), t.TempDir())
			if err != nil {
				t.Fatalf("Execute returned an unexpected error: %v", err)
			}
			if !reflect.DeepEqual(findings, test.want) {
				t.Errorf("findings = %#v, want %#v", findings, test.want)
			}
		})
	}
}

func TestExecuteParsesARealToolsStderr(t *testing.T) {
	command := helperCommand(t, "stderr-report", "go-vet", 0)
	command.Stream = config.StreamStderr

	findings, err := command.Execute(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Execute returned an unexpected error: %v", err)
	}
	want := []Finding{{
		File:     "internal/app/main.go",
		Line:     13,
		Column:   3,
		Rule:     "vet-unreachable",
		Message:  "unreachable code",
		Severity: "MINOR",
		Tool:     "fakelint",
	}}
	if !reflect.DeepEqual(findings, want) {
		t.Errorf("findings = %#v, want %#v", findings, want)
	}
}

func TestExecuteRejectsAnUnknownOutputFormat(t *testing.T) {
	command := Command{
		Name:   "custom-linter",
		Format: "sarif",
		Run: func(context.Context, string, []string) (string, string, int, error) {
			return "something\n", "", 0, nil
		},
	}

	_, err := command.Execute(context.Background(), t.TempDir())
	if err == nil {
		t.Fatal("want an error for an unknown output format, got nil")
	}
	if want := `custom-linter: unknown output format "sarif"`; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %v, want it to mention %q", err, want)
	}
}

func TestExecuteDefaultsToMinorSeverityAndStampsTheToolName(t *testing.T) {
	command := Command{
		Name:   "gofmt",
		Args:   []string{"-l", "."},
		Format: "gofmt-list",
		Run: func(context.Context, string, []string) (string, string, int, error) {
			return "internal/app/main.go\n", "", 0, nil
		},
	}

	findings, err := command.Execute(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Execute returned an unexpected error: %v", err)
	}
	want := []Finding{{
		File:     "internal/app/main.go",
		Rule:     "gofmt",
		Message:  "this file is not gofmt'd",
		Severity: "MINOR",
		Tool:     "gofmt",
	}}
	if !reflect.DeepEqual(findings, want) {
		t.Errorf("findings = %#v, want %#v", findings, want)
	}
}

// lintHelperMode tells the re-executed test binary what to act out.
const lintHelperMode = "CODE_REVIEW_AGENT_LINT_HELPER"

// TestLintHelperProcess is not a test of this package. It is a stand-in for an
// installed linter: the tests re-execute this binary, which then behaves as the
// mode in the environment asks. Every mode exits explicitly so that the test
// framework never prints a PASS line into the output being parsed.
func TestLintHelperProcess(t *testing.T) {
	switch os.Getenv(lintHelperMode) {
	case "report":
		fmt.Fprintln(os.Stdout, "internal/app/main.go:13:3: unreachable code")
		fmt.Fprintln(os.Stderr, "lint: found 1 problem")
		os.Exit(1)
	case "stderr-report":
		// The Go toolchain reports diagnostics on stderr.
		fmt.Fprintln(os.Stderr, "internal/app/main.go:13:3: unreachable code")
		os.Exit(1)
	case "clean":
		fmt.Fprintln(os.Stdout, "[]")
		os.Exit(0)
	case "sleep":
		time.Sleep(20 * time.Second)
		os.Exit(0)
	}
}

func helperCommand(t *testing.T, mode, format string, timeout time.Duration) Command {
	t.Helper()
	t.Setenv(lintHelperMode, mode)
	return Command{
		Name:    "fakelint",
		Args:    []string{os.Args[0], "-test.run=TestLintHelperProcess"},
		Format:  format,
		Timeout: timeout,
	}
}

func TestExecuteRunsARealProcessAndKeepsItsFindings(t *testing.T) {
	command := helperCommand(t, "report", "go-vet", 0)

	findings, err := command.Execute(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Execute returned an unexpected error for a real exit code 1: %v", err)
	}
	want := []Finding{{
		File:     "internal/app/main.go",
		Line:     13,
		Column:   3,
		Rule:     "vet-unreachable",
		Message:  "unreachable code",
		Severity: "MINOR",
		Tool:     "fakelint",
	}}
	if !reflect.DeepEqual(findings, want) {
		t.Errorf("findings = %#v, want %#v", findings, want)
	}
}

func TestExecuteTreatsARealCleanRunAsNoFindings(t *testing.T) {
	command := helperCommand(t, "clean", "ruff-json", 0)

	findings, err := command.Execute(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Execute returned an unexpected error for a clean run: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("findings = %#v, want none", findings)
	}
}

func TestExecuteKillsACommandThatExceedsItsTimeout(t *testing.T) {
	command := helperCommand(t, "sleep", "ruff-json", 500*time.Millisecond)

	start := time.Now()
	_, err := command.Execute(context.Background(), t.TempDir())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("want an error for a tool that had to be killed, got nil")
	}
	if !strings.Contains(err.Error(), "fakelint: exited ") {
		t.Errorf("error = %v, want it to report the exit the kill produced", err)
	}
	if elapsed > 10*time.Second {
		t.Errorf("Execute waited %s, want the tool killed at its 500ms timeout", elapsed)
	}
}

func TestParserNamesAreStable(t *testing.T) {
	// The config file names these formats, so a rename is a breaking change.
	got := make([]string, 0, len(Parsers))
	for format := range Parsers {
		got = append(got, format)
	}
	sort.Strings(got)
	want := []string{"eslint-json", "go-vet", "gofmt-list", "plain", "ruff-json"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parser formats = %q, want %q", got, want)
	}
}
