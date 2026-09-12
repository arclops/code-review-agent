package lint

import (
	"regexp"

	"github.com/arclops/code-review-agent/internal/config"
	"github.com/arclops/code-review-agent/internal/diff"
)

// SecretRule is one pattern for something that must not be committed.
type SecretRule struct {
	Name     string
	Pattern  *regexp.Regexp
	Severity string

	// Ignore marks a match as a placeholder rather than a secret. Only the
	// generic rule has one: a value that looks like a provider's key format is
	// worth reporting whatever it says, but a variable called password is not,
	// when its value is "changeme".
	Ignore *regexp.Regexp
}

// placeholder looks like a stand-in rather than a credential.
var placeholder = regexp.MustCompile(`(?i)(example|placeholder|changeme|change_me|your[_-]|dummy|fake|redacted|sample|xxxx|\*\*\*|\.\.\.|TODO|FIXME|not[_-]?a[_-]?secret|<[^>]*>|\{\{|\$\{|%s)`)

// DefaultSecretRules cover the providers whose key formats are distinctive
// enough to recognise, plus one heuristic for everything else.
//
// They are deliberately narrow. A scanner that cries wolf on every variable
// named token teaches people to ignore it, which is worse than not scanning.
var DefaultSecretRules = []SecretRule{
	{Name: "aws-access-key-id", Severity: config.SeverityCritical,
		Pattern: regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{Name: "aws-secret-access-key", Severity: config.SeverityCritical,
		Pattern: regexp.MustCompile(`(?i)aws_?secret_?access_?key\s*[:=]\s*["']?[A-Za-z0-9/+=]{40}`)},
	{Name: "github-token", Severity: config.SeverityCritical,
		Pattern: regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,}`)},
	{Name: "stripe-secret-key", Severity: config.SeverityCritical,
		Pattern: regexp.MustCompile(`\bsk_live_[0-9a-zA-Z]{16,}`)},
	{Name: "slack-token", Severity: config.SeverityCritical,
		Pattern: regexp.MustCompile(`\bxox[abprs]-[0-9A-Za-z-]{10,}`)},
	{Name: "google-api-key", Severity: config.SeverityCritical,
		Pattern: regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}\b`)},
	{Name: "private-key", Severity: config.SeverityCritical,
		Pattern: regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{Name: "hardcoded-credential", Severity: config.SeverityCritical,
		Pattern: regexp.MustCompile(`(?i)\b(password|passwd|secret|api[_-]?key|access[_-]?token|auth[_-]?token)\b\s*[:=]\s*["'][^"']{8,}["']`),
		Ignore:  placeholder},
}

// ScanPatch looks for secrets in the lines a patch adds.
//
// Only added lines are scanned. A reviewer that reports the credentials already
// in the repository, on every pull request, for ever, is a reviewer nobody
// reads.
func ScanPatch(file, patch string) []Finding {
	return ScanLines(file, diff.AddedLines(patch))
}

// ScanLines looks for secrets in a set of lines that are new to the file.
func ScanLines(file string, lines []diff.Line) []Finding {
	var findings []Finding
	for _, line := range lines {
		for _, rule := range DefaultSecretRules {
			location := rule.Pattern.FindStringIndex(line.Text)
			if location == nil {
				continue
			}
			if rule.Ignore != nil && rule.Ignore.MatchString(line.Text) {
				continue
			}
			findings = append(findings, Finding{
				File: file,
				Line: line.Number,
				// The line's text is deliberately not repeated here. A review
				// comment is public, and copying the credential into it would
				// leak the very thing being reported.
				Rule:     rule.Name,
				Message:  "a value matching " + rule.Name + " was added here; move it to a secret store and rotate it",
				Severity: rule.Severity,
				Tool:     "secret-scan",
			})
		}
	}
	return findings
}
