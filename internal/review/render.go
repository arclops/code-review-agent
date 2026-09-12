package review

import (
	"fmt"
	"sort"
	"strings"

	"github.com/arclops/code-review-agent/internal/config"
	"github.com/arclops/code-review-agent/internal/github"
)

// Render turns a result into the one comment that gets posted.
//
// The order is the point: the worst finding is at the top, because a reader who
// stops after two lines should still see the CRITICAL one. Everything that was
// skipped is listed with its reason, because a reviewer that silently ignores
// half a pull request will be trusted and should not be.
func Render(result Result, pull github.PullRequest) string {
	var out strings.Builder
	out.WriteString(Marker)
	out.WriteString("\n")

	fmt.Fprintf(&out, "## Code review of %s#%d\n\n", result.Repository, result.Number)
	fmt.Fprintf(&out, "%s\n\n", headline(result, pull))

	if len(result.Findings) > 0 {
		for _, severity := range []string{config.SeverityCritical, config.SeverityMajor, config.SeverityMinor} {
			group := findingsAt(result.Findings, severity)
			if len(group) == 0 {
				continue
			}
			fmt.Fprintf(&out, "### %s\n\n", severity)
			for _, finding := range group {
				out.WriteString(findingLine(finding))
			}
			out.WriteString("\n")
		}
	}

	if len(result.Suggestions) > 0 {
		out.WriteString("### Suggestions\n\n")
		for _, suggestion := range dedupe(result.Suggestions) {
			fmt.Fprintf(&out, "- %s\n", suggestion)
		}
		out.WriteString("\n")
	}

	if len(result.Summaries) > 0 {
		out.WriteString("### What the change does\n\n")
		for _, summary := range result.Summaries {
			fmt.Fprintf(&out, "- **`%s`** — %s\n", summary.File, summary.Summary)
		}
		out.WriteString("\n")
	}

	if len(result.FilesSkipped) > 0 {
		fmt.Fprintf(&out, "<details><summary>Skipped %d file(s)</summary>\n\n", len(result.FilesSkipped))
		for _, skipped := range result.FilesSkipped {
			fmt.Fprintf(&out, "- `%s` — %s\n", skipped.File, skipped.Reason)
		}
		out.WriteString("\n</details>\n\n")
	}

	if len(result.ModelErrors) > 0 {
		fmt.Fprintf(&out, "> The model failed for %d file(s), which were reviewed by the tools alone:\n", len(result.ModelErrors))
		for _, failure := range result.ModelErrors {
			fmt.Fprintf(&out, "> - %s\n", failure)
		}
		out.WriteString("\n")
	}

	out.WriteString("---\n\n")
	out.WriteString(footer(result))
	return out.String()
}

// RenderResolved is the comment left behind when a pull request that had
// findings no longer has any. Saying so beats leaving the old ones there.
func RenderResolved(result Result) string {
	var out strings.Builder
	out.WriteString(Marker)
	out.WriteString("\n")
	fmt.Fprintf(&out, "## Code review of %s#%d\n\n", result.Repository, result.Number)
	fmt.Fprintf(&out, "No findings on `%s`. Everything reported here before has been fixed or removed.\n\n", shortSHA(result.HeadSHA))
	out.WriteString("---\n\n")
	out.WriteString(footer(result))
	return out.String()
}

func headline(result Result, pull github.PullRequest) string {
	counts := map[string]int{}
	for _, finding := range result.Findings {
		counts[finding.Severity]++
	}

	parts := []string{fmt.Sprintf("**%d file(s) reviewed** at `%s`", result.FilesReviewed, shortSHA(result.HeadSHA))}
	if len(result.Findings) == 0 {
		parts = append(parts, "no findings")
	} else {
		var breakdown []string
		for _, severity := range []string{config.SeverityCritical, config.SeverityMajor, config.SeverityMinor} {
			if counts[severity] > 0 {
				breakdown = append(breakdown, fmt.Sprintf("%d %s", counts[severity], severity))
			}
		}
		parts = append(parts, fmt.Sprintf("%d finding(s): %s", len(result.Findings), strings.Join(breakdown, ", ")))
	}
	if len(result.FilesSkipped) > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped", len(result.FilesSkipped)))
	}
	return strings.Join(parts, " · ")
}

func findingLine(finding Finding) string {
	location := fmt.Sprintf("`%s`", finding.File)
	if finding.Line > 0 {
		location = fmt.Sprintf("`%s:%d`", finding.File, finding.Line)
	}
	var out strings.Builder
	fmt.Fprintf(&out, "- %s **%s**", location, finding.Title)
	if finding.Source != "" {
		fmt.Fprintf(&out, " _(%s)_", finding.Source)
	}
	out.WriteString("\n")
	if detail := strings.TrimSpace(finding.Detail); detail != "" && !strings.EqualFold(detail, finding.Title) {
		for _, line := range strings.Split(detail, "\n") {
			fmt.Fprintf(&out, "  %s\n", strings.TrimSpace(line))
		}
	}
	return out.String()
}

func footer(result Result) string {
	var parts []string
	if verdict := strings.TrimSpace(result.Verdict); verdict != "" {
		parts = append(parts, "Verdict: "+verdict)
	}
	if len(result.Usage) > 0 {
		prompt, completion := 0, 0
		for _, usage := range result.Usage {
			prompt += usage.PromptTokens
			completion += usage.CompletionTokens
		}
		summary := result.Usage[0].Model
		if prompt+completion > 0 {
			summary += fmt.Sprintf(", %d prompt + %d completion tokens", prompt, completion)
		}
		parts = append(parts, "model: "+summary)
	}
	if result.Guidelines > 0 {
		parts = append(parts, fmt.Sprintf("%d guideline(s)", result.Guidelines))
	}
	return strings.Join(parts, " · ")
}

func findingsAt(findings []Finding, severity string) []Finding {
	var group []Finding
	for _, finding := range findings {
		if finding.Severity == severity {
			group = append(group, finding)
		}
	}
	return group
}

// dedupe removes repeated suggestions, keeping the first.
func dedupe(values []string) []string {
	seen := make(map[string]bool, len(values))
	var unique []string
	for _, value := range values {
		key := strings.ToLower(strings.TrimSpace(value))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		unique = append(unique, strings.TrimSpace(value))
	}
	return unique
}

// SortFindings orders findings for display. It is exported for the tests and
// for anything that wants the same order outside a comment.
func SortFindings(findings []Finding) []Finding {
	sorted := append([]Finding(nil), findings...)
	sort.SliceStable(sorted, func(i, j int) bool {
		left, right := config.SeverityOrder[sorted[i].Severity], config.SeverityOrder[sorted[j].Severity]
		if left != right {
			return left < right
		}
		if sorted[i].File != sorted[j].File {
			return sorted[i].File < sorted[j].File
		}
		return sorted[i].Line < sorted[j].Line
	})
	return sorted
}
