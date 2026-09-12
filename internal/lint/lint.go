// Package lint runs the deterministic checks: the static analysis tools the
// repository is set up for, and a secret scan.
//
// These are the findings a model must not be asked to guess at. A model asked
// to spot an unused import is doing an expensive impression of a linter and is
// worse at it, so the tools report and the model interprets.
package lint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/arclops/code-review-agent/internal/config"
)

// Finding is one thing a tool reported.
type Finding struct {
	File     string
	Line     int
	Column   int
	Rule     string
	Message  string
	Severity string
	Tool     string
}

// Parser turns a tool's output into findings.
type Parser func(stdout []byte, severity func(string) string) ([]Finding, error)

// Parsers are the output formats that are understood. A tool that emits
// something else can still be used with the plain parser, which reports one
// finding per non-empty line.
var Parsers = map[string]Parser{
	"ruff-json":   parseRuff,
	"eslint-json": parseESLint,
	"go-vet":      parseGoVet,
	"gofmt-list":  parseGofmtList,
	"plain":       parsePlain,
}

// Command is one tool to run.
type Command struct {
	Name    string
	Args    []string
	Format  string
	Timeout time.Duration

	// Stream names the stream that carries the tool's findings: stdout (the
	// default), stderr, or both. It exists because the Go toolchain reports
	// diagnostics on stderr, so reading only stdout makes go vet look clean.
	Stream string

	// Severity maps a rule to a severity level. The zero value reports
	// everything as minor, which is the safe direction to be wrong in.
	Severity func(rule string) string

	// Run executes the tool. It exists so that a test does not need the tool
	// installed, and so that the same runner works in a container.
	Run func(ctx context.Context, dir string, args []string) (stdout, stderr string, exitCode int, err error)
}

// Execute runs one tool in dir and returns its findings.
//
// A linter exits non-zero when it finds something, which is not a failure: a
// runner that treated a non-zero exit as an error would throw away exactly the
// output it was run for.
func (c Command) Execute(ctx context.Context, dir string) ([]Finding, error) {
	if c.Run == nil {
		c.Run = execCommand
	}
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}

	stdout, stderr, exitCode, err := c.Run(ctx, dir, c.Args)
	if err != nil {
		return nil, fmt.Errorf("%s: could not run: %w", c.Name, err)
	}

	parser, ok := Parsers[c.Format]
	if !ok {
		return nil, fmt.Errorf("%s: unknown output format %q", c.Name, c.Format)
	}
	severity := c.Severity
	if severity == nil {
		severity = func(string) string { return config.SeverityMinor }
	}
	output := c.output(stdout, stderr)
	findings, parseErr := parser([]byte(output), severity)
	for index := range findings {
		findings[index].Tool = c.Name
	}
	if parseErr != nil {
		// A tool that failed for its own reasons is worth saying out loud
		// rather than reporting as a clean run.
		if exitCode != 0 && len(strings.TrimSpace(output)) == 0 {
			return nil, fmt.Errorf("%s: exited %d: %s", c.Name, exitCode, truncate(stderr, 300))
		}
		return nil, fmt.Errorf("%s: could not read its output: %w", c.Name, parseErr)
	}
	return findings, nil
}

// output picks the text to parse. Concatenating the two streams is only safe
// for the line oriented formats, which is why the choice is made per command
// rather than guessed at here.
func (c Command) output(stdout, stderr string) string {
	switch c.Stream {
	case config.StreamStderr:
		return stderr
	case config.StreamBoth:
		return stdout + "\n" + stderr
	default:
		return stdout
	}
}

func execCommand(ctx context.Context, dir string, args []string) (string, string, int, error) {
	if len(args) == 0 {
		return "", "", 0, fmt.Errorf("no command to run")
	}
	command := exec.CommandContext(ctx, args[0], args[1:]...)
	command.Dir = dir
	var stdout, stderr strings.Builder
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	exitCode := 0
	if err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			exitCode = exitError.ExitCode()
		} else {
			return stdout.String(), stderr.String(), 0, err
		}
	}
	return stdout.String(), stderr.String(), exitCode, nil
}

func parseRuff(stdout []byte, severity func(string) string) ([]Finding, error) {
	var reported []struct {
		Filename string `json:"filename"`
		Code     string `json:"code"`
		Message  string `json:"message"`
		Location struct {
			Row    int `json:"row"`
			Column int `json:"column"`
		} `json:"location"`
	}
	if err := json.Unmarshal(stdout, &reported); err != nil {
		return nil, fmt.Errorf("not ruff JSON: %w", err)
	}
	findings := make([]Finding, 0, len(reported))
	for _, item := range reported {
		findings = append(findings, Finding{
			File:     item.Filename,
			Line:     item.Location.Row,
			Column:   item.Location.Column,
			Rule:     item.Code,
			Message:  item.Message,
			Severity: severity(item.Code),
		})
	}
	return findings, nil
}

func parseESLint(stdout []byte, severity func(string) string) ([]Finding, error) {
	var reported []struct {
		FilePath string `json:"filePath"`
		Messages []struct {
			Line     int    `json:"line"`
			Column   int    `json:"column"`
			RuleID   string `json:"ruleId"`
			Message  string `json:"message"`
			Severity int    `json:"severity"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(stdout, &reported); err != nil {
		return nil, fmt.Errorf("not eslint JSON: %w", err)
	}
	var findings []Finding
	for _, file := range reported {
		for _, message := range file.Messages {
			rule := message.RuleID
			if rule == "" {
				rule = "parse-error"
			}
			findings = append(findings, Finding{
				File:     file.FilePath,
				Line:     message.Line,
				Column:   message.Column,
				Rule:     rule,
				Message:  message.Message,
				Severity: severity(rule),
			})
		}
	}
	return findings, nil
}

// vetLine matches "app/config.go:12:5: message".
var vetLine = regexp.MustCompile(`^(.+?):(\d+):(?:(\d+):)?\s*(.+)$`)

func parseGoVet(stdout []byte, severity func(string) string) ([]Finding, error) {
	var findings []Finding
	for _, line := range strings.Split(string(stdout), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		match := vetLine.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		lineNumber, _ := strconv.Atoi(match[2])
		column := 0
		if match[3] != "" {
			column, _ = strconv.Atoi(match[3])
		}
		message := match[4]
		findings = append(findings, Finding{
			File:     match[1],
			Line:     lineNumber,
			Column:   column,
			Rule:     vetRule(message),
			Message:  message,
			Severity: severity(vetRule(message)),
		})
	}
	return findings, nil
}

// vetRule turns a vet message into something stable enough to group by.
func vetRule(message string) string {
	switch {
	case strings.Contains(message, "possible misuse"):
		return "vet-misuse"
	case strings.Contains(message, "Printf"):
		return "vet-printf"
	case strings.Contains(message, "unreachable"):
		return "vet-unreachable"
	case strings.Contains(message, "lostcancel"):
		return "vet-lostcancel"
	case strings.Contains(message, "copylocks"):
		return "vet-copylocks"
	default:
		return "vet"
	}
}

func parseGofmtList(stdout []byte, severity func(string) string) ([]Finding, error) {
	var findings []Finding
	for _, line := range strings.Split(string(stdout), "\n") {
		file := strings.TrimSpace(line)
		if file == "" {
			continue
		}
		findings = append(findings, Finding{
			File:     file,
			Rule:     "gofmt",
			Message:  "this file is not gofmt'd",
			Severity: severity("gofmt"),
		})
	}
	return findings, nil
}

func parsePlain(stdout []byte, _ func(string) string) ([]Finding, error) {
	var findings []Finding
	for _, line := range strings.Split(string(stdout), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		findings = append(findings, Finding{Rule: "output", Message: line})
	}
	return findings, nil
}

func truncate(text string, limit int) string {
	text = strings.TrimSpace(text)
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "..."
}
