// Package review is the pipeline: fetch a pull request, decide what is worth
// reviewing, run the deterministic tools, ask the model about the rest, merge
// everything into one ordered comment and post it.
//
// The order of those steps matters. The tools run first, because their findings
// are facts the model is then told not to argue with, and because they are
// still worth posting when the model is unavailable.
package review

import (
	"context"
	"fmt"
	"io"
	"log"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/arclops/code-review-agent/internal/config"
	"github.com/arclops/code-review-agent/internal/diff"
	"github.com/arclops/code-review-agent/internal/github"
	"github.com/arclops/code-review-agent/internal/lint"
	"github.com/arclops/code-review-agent/internal/model"
)

// Marker identifies the comment this service owns, so that a later run updates
// it rather than adding another one. Three reviews of three pushes is exactly
// what the challenge warns about.
const Marker = "<!-- code-review-agent -->"

// GitHub is the part of the API this package uses.
type GitHub interface {
	PullRequest(ctx context.Context, repo string, number int) (github.PullRequest, error)
	Files(ctx context.Context, repo string, number int) ([]github.File, error)
	Comments(ctx context.Context, repo string, number int) ([]github.Comment, error)
	CreateComment(ctx context.Context, repo string, number int, body string) (github.Comment, error)
	UpdateComment(ctx context.Context, repo string, commentID int64, body string) (github.Comment, error)
}

// CheckoutFn fetches a revision and returns the directory plus a cleanup.
type CheckoutFn func(ctx context.Context, url, ref string) (string, func(), error)

// Options are the collaborators the pipeline needs.
type Options struct {
	Config   config.Config
	GitHub   GitHub
	Model    model.Provider
	Checkout CheckoutFn
	Logger   *log.Logger
	Now      func() time.Time

	// BotLogin is the account the bot posts as, used to recognise its own
	// comments when the marker is missing.
	BotLogin string
}

// Skipped is a file that was not reviewed, and why.
type Skipped struct {
	File   string
	Reason string
}

// Finding is one thing the review reports.
type Finding struct {
	Severity string
	File     string
	Line     int
	Title    string
	Detail   string
	Source   string
}

// Result is everything the run produced.
type Result struct {
	Repository    string
	Number        int
	HeadSHA       string
	FilesReviewed int
	FilesSkipped  []Skipped
	Findings      []Finding
	Summaries     []FileSummary
	Suggestions   []string
	Verdict       string
	Usage         []model.Usage
	ModelErrors   []string
	Guidelines    int

	Comment   string
	Posted    bool
	Updated   bool
	CommentID int64
	Reason    string
}

// FileSummary is the model's short description of one file.
type FileSummary struct {
	File    string
	Summary string
}

// Run performs one review of one pull request.
func Run(ctx context.Context, repo string, number int, options Options) (Result, error) {
	if options.Logger == nil {
		options.Logger = log.New(io.Discard, "", 0)
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Model == nil {
		options.Model = model.None{}
	}

	result := Result{Repository: repo, Number: number, Guidelines: len(options.Config.Guidelines)}

	pull, err := options.GitHub.PullRequest(ctx, repo, number)
	if err != nil {
		return result, fmt.Errorf("could not read pull request %s#%d: %w", repo, number, err)
	}
	result.HeadSHA = pull.Head.SHA

	files, err := options.GitHub.Files(ctx, repo, number)
	if err != nil {
		return result, fmt.Errorf("could not read the files of %s#%d: %w", repo, number, err)
	}
	options.Logger.Printf("pull request %s#%d changes %d file(s) at %s", repo, number, len(files), shortSHA(pull.Head.SHA))

	planned, skipped := FilterFiles(options.Config.Filters, files)
	result.FilesSkipped = skipped
	result.FilesReviewed = len(planned)

	// --- deterministic findings -------------------------------------------
	findings := secretFindings(planned)
	findings = append(findings, toolFindings(ctx, repo, pull, planned, options)...)
	options.Logger.Printf("deterministic checks found %d finding(s)", len(findings))

	// --- the model ---------------------------------------------------------
	modelFindings, summaries, suggestions, verdict, usage, modelErrors := reviewWithModel(ctx, repo, pull, planned, findings, options)
	result.Summaries = summaries
	result.Suggestions = suggestions
	result.Verdict = verdict
	result.Usage = usage
	result.ModelErrors = modelErrors

	findings = append(findings, modelFindings...)
	result.Findings = mergeAndSort(findings, options.Logger)

	result.Comment = Render(result, pull)
	return result, nil
}

// Post decides whether the review is worth posting and does it.
//
// A review that found nothing does not get a comment: a bot that says "looks
// good to me" on every trivial pull request is muted, and a muted bot is worth
// nothing. If it posted something before, that comment is updated to say the
// findings are gone, because leaving stale findings on a pull request is worse
// than saying nothing.
func Post(ctx context.Context, result *Result, options Options) error {
	if options.Logger == nil {
		options.Logger = log.New(io.Discard, "", 0)
	}

	comments, err := options.GitHub.Comments(ctx, result.Repository, result.Number)
	if err != nil {
		return fmt.Errorf("could not read the existing comments: %w", err)
	}
	existing := findOwnComment(comments, options.BotLogin)

	// A model that failed for every file means the review is incomplete, and
	// saying nothing would make a broken review look like a clean one.
	if len(result.Findings) == 0 {
		switch {
		case len(result.ModelErrors) > 0 && result.FilesReviewed > 0:
			result.Reason = fmt.Sprintf("the model failed for every file (%d error(s)); the deterministic checks were clean, so nothing was posted",
				len(result.ModelErrors))
			return fmt.Errorf("the model failed for every file and nothing was found: %s", strings.Join(result.ModelErrors, "; "))
		case existing != nil:
			body := RenderResolved(*result)
			comment, err := options.GitHub.UpdateComment(ctx, result.Repository, existing.ID, body)
			if err != nil {
				return fmt.Errorf("could not update the existing comment: %w", err)
			}
			result.CommentID = comment.ID
			result.Updated = true
			result.Posted = true
			result.Reason = "nothing to report, and an earlier review was on the pull request: it was updated to say so"
			options.Logger.Printf("%s#%d: %s", result.Repository, result.Number, result.Reason)
			return nil
		default:
			result.Reason = "the review found nothing to report, so no comment was posted"
			options.Logger.Printf("%s#%d: %s", result.Repository, result.Number, result.Reason)
			return nil
		}
	}

	if existing != nil {
		comment, err := options.GitHub.UpdateComment(ctx, result.Repository, existing.ID, result.Comment)
		if err != nil {
			return fmt.Errorf("could not update the existing comment: %w", err)
		}
		result.CommentID = comment.ID
		result.Updated = true
	} else {
		comment, err := options.GitHub.CreateComment(ctx, result.Repository, result.Number, result.Comment)
		if err != nil {
			return fmt.Errorf("could not post the comment: %w", err)
		}
		result.CommentID = comment.ID
	}
	result.Posted = true
	options.Logger.Printf("%s#%d: posted %d finding(s)", result.Repository, result.Number, len(result.Findings))
	return nil
}

// FilterFiles decides which changed files are worth reviewing, and records a
// reason for every one it skips. A reviewer that silently ignores half a pull
// request is worse than no reviewer, because it will be trusted.
func FilterFiles(filters config.FiltersConfig, files []github.File) ([]github.File, []Skipped) {
	var planned []github.File
	var skipped []Skipped

	for _, file := range files {
		// The reason a file was skipped should be about the file. "Only the
		// first 200 files are reviewed" is true of a lockfile too, but it is
		// not why the lockfile was skipped, so the cap is checked last.
		switch {
		case file.Status == "removed" && !filters.IncludeDeleted:
			skipped = append(skipped, Skipped{File: file.Filename, Reason: "the file was deleted"})
		case strings.TrimSpace(file.Patch) == "":
			skipped = append(skipped, Skipped{File: file.Filename, Reason: "no diff: a binary file, or one GitHub does not diff"})
		case matchesAny(filters.SkipPatterns, file.Filename):
			skipped = append(skipped, Skipped{File: file.Filename,
				Reason: "generated, vendored or locked: matches " + firstMatch(filters.SkipPatterns, file.Filename)})
		case filters.MaxPatchBytes > 0 && len(file.Patch) > filters.MaxPatchBytes:
			skipped = append(skipped, Skipped{File: file.Filename,
				Reason: fmt.Sprintf("the diff is %d bytes, over the %d byte limit", len(file.Patch), filters.MaxPatchBytes)})
		case filters.MaxFiles > 0 && len(planned) >= filters.MaxFiles:
			skipped = append(skipped, Skipped{File: file.Filename,
				Reason: fmt.Sprintf("only the first %d files are reviewed", filters.MaxFiles)})
		default:
			planned = append(planned, file)
		}
	}
	return planned, skipped
}

// secretFindings scans the added lines of every file. It needs no checkout,
// which means the one finding that must never be missed does not depend on a
// clone succeeding.
func secretFindings(files []github.File) []Finding {
	var findings []Finding
	for _, file := range files {
		for _, found := range lint.ScanPatch(file.Filename, file.Patch) {
			findings = append(findings, Finding{
				Severity: found.Severity,
				File:     found.File,
				Line:     found.Line,
				Title:    found.Rule,
				Detail:   found.Message,
				Source:   found.Tool,
			})
		}
	}
	return findings
}

// toolFindings checks the code out and runs the configured static analysis over
// it, keeping only what the pull request itself introduced.
func toolFindings(ctx context.Context, repo string, pull github.PullRequest, planned []github.File, options Options) []Finding {
	if !options.Config.Checkout.Enabled || len(options.Config.Lint.Commands) == 0 || options.Checkout == nil {
		return nil
	}

	url := fmt.Sprintf("https://github.com/%s.git", repo)
	dir, cleanup, err := options.Checkout(ctx, url, pull.Head.SHA)
	if err != nil {
		// A checkout that fails costs the linter, not the review: the secret
		// scan and the model still have something to say.
		options.Logger.Printf("could not check out %s at %s: %v", repo, shortSHA(pull.Head.SHA), err)
		return nil
	}
	defer cleanup()

	touched := make(map[string]map[int]bool, len(planned))
	inPullRequest := make(map[string]bool, len(planned))
	for _, file := range planned {
		touched[file.Filename] = diff.Touched(file.Patch)
		inPullRequest[file.Filename] = true
	}

	var findings []Finding
	for _, command := range options.Config.Lint.Commands {
		runner := lint.Command{
			Name:     command.Name,
			Args:     command.Command,
			Format:   command.Format,
			Stream:   command.Stream,
			Timeout:  options.Config.Limits.CallTimeout.Duration(),
			Severity: options.Config.SeverityFor,
		}
		found, err := runner.Execute(ctx, dir)
		if err != nil {
			options.Logger.Printf("linter %s could not run: %v", command.Name, err)
			continue
		}
		kept := 0
		for _, item := range found {
			file := normalizePath(item.File, dir)
			if !inPullRequest[file] {
				continue
			}
			// A finding on a line the pull request did not touch is somebody
			// else's problem, and reporting it here would bury the change.
			if item.Line > 0 && !touched[file][item.Line] {
				continue
			}
			findings = append(findings, Finding{
				Severity: item.Severity,
				File:     file,
				Line:     item.Line,
				Title:    item.Rule,
				Detail:   item.Message,
				Source:   item.Tool,
			})
			kept++
		}
		options.Logger.Printf("linter %s reported %d finding(s), %d in the changed lines",
			command.Name, len(found), kept)
	}
	return findings
}

// modelOutcome is one file's model review.
type modelOutcome struct {
	review model.Review
	usage  model.Usage
	err    error
}

// reviewWithModel asks the model about every planned file, several at a time.
func reviewWithModel(ctx context.Context, repo string, pull github.PullRequest, planned []github.File, facts []Finding, options Options) (
	[]Finding, []FileSummary, []string, string, []model.Usage, []string) {

	if _, isNone := options.Model.(model.None); isNone {
		return nil, nil, nil, "not reviewed by a model: none is configured", nil, nil
	}

	concurrency := options.Config.Limits.PerFileConcurrenc
	if concurrency < 1 {
		concurrency = 1
	}

	// Findings are indexed by file so that each review is given its own facts.
	byFile := make(map[string][]model.Finding)
	for _, finding := range facts {
		byFile[finding.File] = append(byFile[finding.File], model.Finding{
			File: finding.File, Line: finding.Line, Rule: finding.Title,
			Message: finding.Detail, Severity: finding.Severity, Tool: finding.Source,
		})
	}

	outcomes := make([]modelOutcome, len(planned))
	semaphore := make(chan struct{}, concurrency)
	var wait sync.WaitGroup

	started := time.Now()
	for index, file := range planned {
		wait.Add(1)
		go func(index int, file github.File) {
			defer wait.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			request := model.Request{
				Repository:  repo,
				Number:      pull.Number,
				Title:       pull.Title,
				Description: pull.Body,
				Author:      pull.User.Login,
				File:        file.Filename,
				Language:    languageOf(file.Filename),
				Patch:       file.Patch,
				Findings:    byFile[file.Filename],
				Guidelines:  options.Config.Guidelines,
			}
			review, usage, err := options.Model.Review(ctx, request)
			outcomes[index] = modelOutcome{review: review, usage: usage, err: err}
		}(index, file)
	}
	wait.Wait()
	options.Logger.Printf("reviewed %d file(s) with %s in %s", len(planned), options.Model.Name(), time.Since(started).Round(time.Millisecond))

	var findings []Finding
	var summaries []FileSummary
	var suggestions []string
	var usage []model.Usage
	var failures []string
	verdict := ""

	for index, outcome := range outcomes {
		file := planned[index].Filename
		if outcome.err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", file, outcome.err))
			options.Logger.Printf("model review of %s failed: %v", file, outcome.err)
			continue
		}
		usage = append(usage, outcome.usage)
		if outcome.review.Summary != "" {
			summaries = append(summaries, FileSummary{File: file, Summary: outcome.review.Summary})
		}
		for _, issue := range outcome.review.Issues {
			findings = append(findings, Finding{
				Severity: issue.Severity,
				File:     file,
				Line:     issue.Line,
				Title:    issue.Title,
				Detail:   issue.Detail,
				Source:   "review",
			})
		}
		suggestions = append(suggestions, outcome.review.Suggestions...)
		if verdict == "" || worseVerdict(outcome.review.Verdict, verdict) {
			verdict = outcome.review.Verdict
		}
	}
	return findings, summaries, suggestions, verdict, usage, failures
}

// mergeAndSort removes what the tools already said, and puts the worst first.
func mergeAndSort(findings []Finding, logger *log.Logger) []Finding {
	linterLines := make(map[string]bool)
	for _, finding := range findings {
		if finding.Source != "review" {
			linterLines[fmt.Sprintf("%s:%d", finding.File, finding.Line)] = true
		}
	}

	seen := make(map[string]bool)
	merged := findings[:0]
	for _, finding := range findings {
		key := fmt.Sprintf("%s:%d:%s", finding.File, finding.Line, strings.ToLower(finding.Title))
		if seen[key] {
			continue
		}
		// The model repeating a linter's finding is dropped: the tool's version
		// is precise, and the model was asked not to do this anyway.
		if finding.Source == "review" && linterLines[fmt.Sprintf("%s:%d", finding.File, finding.Line)] {
			logger.Printf("dropped a model finding that repeats a tool finding at %s:%d", finding.File, finding.Line)
			continue
		}
		seen[key] = true
		merged = append(merged, finding)
	}

	sort.SliceStable(merged, func(i, j int) bool {
		left, right := config.SeverityOrder[merged[i].Severity], config.SeverityOrder[merged[j].Severity]
		if left != right {
			return left < right
		}
		if merged[i].File != merged[j].File {
			return merged[i].File < merged[j].File
		}
		return merged[i].Line < merged[j].Line
	})
	return merged
}

// findOwnComment finds this service's previous comment on the pull request.
func findOwnComment(comments []github.Comment, botLogin string) *github.Comment {
	for index := range comments {
		if strings.Contains(comments[index].Body, Marker) {
			return &comments[index]
		}
	}
	if botLogin != "" {
		for index := range comments {
			if strings.EqualFold(comments[index].User.Login, botLogin) {
				return &comments[index]
			}
		}
	}
	return nil
}

// matchesAny reports whether a path matches any of the patterns, which may use
// a leading or trailing star.
func matchesAny(patterns []string, file string) bool {
	return firstMatch(patterns, file) != ""
}

// firstMatch returns the first pattern that matched, so that the skip reason
// can name the rule rather than gesture at it.
func firstMatch(patterns []string, file string) string {
	for _, pattern := range patterns {
		if matchPattern(pattern, file) {
			return pattern
		}
	}
	return ""
}

func matchPattern(pattern, file string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return false
	}
	// A pattern without a slash matches the file name at any depth, which is
	// what somebody writing "*.lock" means.
	if !strings.Contains(pattern, "/") {
		if matched, err := path.Match(pattern, path.Base(file)); err == nil && matched {
			return true
		}
	}
	if strings.HasPrefix(pattern, "*") {
		return strings.HasSuffix(file, strings.TrimPrefix(pattern, "*"))
	}
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(file, strings.TrimSuffix(pattern, "*"))
	}
	if matched, err := path.Match(pattern, file); err == nil && matched {
		return true
	}
	return file == pattern
}

// normalizePath turns a path a tool printed into a repository relative one.
func normalizePath(file, root string) string {
	file = strings.TrimSpace(file)
	file = strings.TrimPrefix(file, "./")
	file = strings.ReplaceAll(file, "\\", "/")
	if root != "" {
		trimmed := strings.TrimSuffix(strings.ReplaceAll(root, "\\", "/"), "/")
		file = strings.TrimPrefix(file, trimmed+"/")
	}
	return file
}

func languageOf(file string) string {
	switch path.Ext(file) {
	case ".go":
		return "go"
	case ".py":
		return "python"
	case ".js", ".mjs", ".cjs":
		return "javascript"
	case ".ts":
		return "typescript"
	case ".rb":
		return "ruby"
	case ".rs":
		return "rust"
	case ".java":
		return "java"
	case ".md":
		return "markdown"
	default:
		return ""
	}
}

func worseVerdict(candidate, current string) bool {
	rank := map[string]int{"approve": 0, "comment": 1, "": 1, "request changes": 2, "request_changes": 2, "reject": 3}
	return rank[strings.ToLower(strings.TrimSpace(candidate))] > rank[strings.ToLower(strings.TrimSpace(current))]
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
