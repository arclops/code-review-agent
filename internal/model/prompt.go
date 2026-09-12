package model

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/arclops/code-review-agent/internal/config"
)

// The system prompt is where the division of labour between the linter and the
// model is stated. Everything else in this package is plumbing around it.
const systemPrompt = `You are an experienced engineer reviewing a pull request. You are given one changed file at a time.

Reply with a single JSON object and nothing else:
{
  "summary": "one or two sentences about this file",
  "issues": [
    {"severity": "CRITICAL", "line": 12, "title": "short title", "detail": "why it matters and what to do"}
  ],
  "suggestions": ["an idea worth considering"],
  "verdict": "approve"
}

Severity means: CRITICAL for a committed secret, a security hole or data loss. MAJOR for a bug, a broken contract or an unhandled failure. MINOR for style, naming and structure.

Rules you must follow:
- The static analysis findings listed below are facts produced by real tools. Do not repeat them as your own findings, do not reword them, and do not contradict them. They are reported separately, with their tool and rule names.
- Spend your own attention on what a linter cannot judge: whether the change does what the pull request says it does, whether the naming means anything, whether the approach is sound, and whether something is missing.
- Use the line numbers of the new file, which the diff shows in its hunk headers.
- If there is nothing worth saying about this file, return an empty issues array and a short summary. Do not invent problems to look useful: a review that complains about correct code gets muted.
- Reply with JSON only. No code fences, no commentary before or after.`

// Prompt builds the two messages for one file.
func Prompt(request Request) (system string, user string) {
	var builder strings.Builder

	fmt.Fprintf(&builder, "Repository: %s\n", request.Repository)
	fmt.Fprintf(&builder, "Pull request #%d: %s\n", request.Number, request.Title)
	if request.Author != "" {
		fmt.Fprintf(&builder, "Author: %s\n", request.Author)
	}
	if strings.TrimSpace(request.Description) != "" {
		fmt.Fprintf(&builder, "\nWhat the author says it does:\n%s\n", truncateString(request.Description, 4000))
	}

	if len(request.Guidelines) > 0 {
		builder.WriteString("\nThe team's review guidelines:\n")
		for index, guideline := range request.Guidelines {
			fmt.Fprintf(&builder, "%d. %s\n", index+1, guideline)
		}
	}

	fmt.Fprintf(&builder, "\nFile under review: %s\n", request.File)
	if request.Language != "" {
		fmt.Fprintf(&builder, "Language: %s\n", request.Language)
	}

	builder.WriteString("\nStatic analysis findings for this file:\n")
	if len(request.Findings) == 0 {
		builder.WriteString("none\n")
	} else {
		for _, finding := range request.Findings {
			fmt.Fprintf(&builder, "- line %d [%s] %s (%s): %s\n",
				finding.Line, finding.Rule, finding.Tool, finding.Severity, finding.Message)
		}
	}

	fmt.Fprintf(&builder, "\nDiff of this file:\n%s\n", request.Patch)
	return systemPrompt, builder.String()
}

// ParseReview reads the JSON a model was asked for.
//
// Models wrap JSON in code fences, add a sentence before it, or return the
// object inside prose. None of that is worth failing a review over, so the
// first balanced object in the reply is taken.
func ParseReview(text string) (Review, error) {
	object, ok := ExtractJSON(text)
	if !ok {
		return Review{}, fmt.Errorf("model: the reply contained no JSON object: %s", truncateString(text, 300))
	}

	var review Review
	if err := json.Unmarshal([]byte(object), &review); err != nil {
		return Review{}, fmt.Errorf("model: the reply is not the JSON that was asked for: %w", err)
	}

	// Normalise what the model said into what the comment can render.
	cleaned := review.Issues[:0]
	for _, issue := range review.Issues {
		issue.Severity = normaliseSeverity(issue.Severity)
		issue.Title = strings.TrimSpace(issue.Title)
		issue.Detail = strings.TrimSpace(issue.Detail)
		if issue.Title == "" && issue.Detail == "" {
			continue
		}
		if issue.Title == "" {
			issue.Title = firstLine(issue.Detail)
		}
		cleaned = append(cleaned, issue)
	}
	review.Issues = cleaned
	review.Summary = strings.TrimSpace(review.Summary)
	review.Verdict = strings.TrimSpace(review.Verdict)

	suggestions := review.Suggestions[:0]
	for _, suggestion := range review.Suggestions {
		if trimmed := strings.TrimSpace(suggestion); trimmed != "" {
			suggestions = append(suggestions, trimmed)
		}
	}
	review.Suggestions = suggestions
	return review, nil
}

func normaliseSeverity(value string) string {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case config.SeverityCritical, "BLOCKER", "HIGH", "ERROR":
		return config.SeverityCritical
	case config.SeverityMajor, "MEDIUM", "WARNING", "WARN":
		return config.SeverityMajor
	case config.SeverityMinor, "LOW", "INFO", "NIT", "SUGGESTION":
		return config.SeverityMinor
	default:
		return config.SeverityMinor
	}
}

// reviewKeys are the fields the prompt asks for. A brace pair that contains
// none of them is far more likely to be part of the prose around the answer
// than the answer itself: models explain the schema before they fill it in.
var reviewKeys = []string{"summary", "issues", "suggestions", "verdict"}

// ExtractJSON returns the JSON object in a string, ignoring braces inside
// strings, code fences and whatever the model wrote around it.
//
// It looks for the first balanced object that is one of these things, in order:
// an object carrying one of the fields the prompt asked for, then any valid
// JSON object at all. Preferring the answer over a mention of the schema is the
// difference between a review and a silently empty one.
func ExtractJSON(text string) (string, bool) {
	firstValid := ""

	for from := 0; from < len(text); from++ {
		start := strings.IndexByte(text[from:], '{')
		if start < 0 {
			break
		}
		start += from

		candidate, ok := balancedObject(text[start:])
		if !ok {
			break
		}
		if !json.Valid([]byte(candidate)) {
			from = start
			continue
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(candidate), &decoded); err != nil {
			from = start
			continue
		}
		for _, key := range reviewKeys {
			if _, ok := decoded[key]; ok {
				return candidate, true
			}
		}
		if firstValid == "" {
			firstValid = candidate
		}
		from = start
	}

	if firstValid != "" {
		return firstValid, true
	}
	return "", false
}

// balancedObject returns the object that starts at the beginning of text, and
// whether it is balanced at all.
func balancedObject(text string) (string, bool) {
	depth := 0
	inString := false
	escaped := false
	for index := 0; index < len(text); index++ {
		character := text[index]
		switch {
		case inString:
			switch {
			case escaped:
				escaped = false
			case character == '\\':
				escaped = true
			case character == '"':
				inString = false
			}
		case character == '"':
			inString = true
		case character == '{':
			depth++
		case character == '}':
			depth--
			if depth == 0 {
				return text[:index+1], true
			}
		}
	}
	return "", false
}

func firstLine(text string) string {
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		return strings.TrimSpace(text[:index])
	}
	return text
}

func truncateString(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "..."
}
