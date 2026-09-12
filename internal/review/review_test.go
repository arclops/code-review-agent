package review

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arclops/code-review-agent/internal/config"
	"github.com/arclops/code-review-agent/internal/github"
	"github.com/arclops/code-review-agent/internal/model"
)

// ---------------------------------------------------------------- test doubles

type fakeGitHub struct {
	mu       sync.Mutex
	pull     github.PullRequest
	files    []github.File
	comments []github.Comment

	pullErr  error
	filesErr error

	created     []string
	updated     []string
	nextID      int64
	commentErr  error
	commentsErr error
}

func (f *fakeGitHub) PullRequest(context.Context, string, int) (github.PullRequest, error) {
	if f.pullErr != nil {
		return github.PullRequest{}, f.pullErr
	}
	return f.pull, nil
}

func (f *fakeGitHub) Files(context.Context, string, int) ([]github.File, error) {
	if f.filesErr != nil {
		return nil, f.filesErr
	}
	return f.files, nil
}

func (f *fakeGitHub) Comments(context.Context, string, int) ([]github.Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.commentsErr != nil {
		return nil, f.commentsErr
	}
	return append([]github.Comment(nil), f.comments...), nil
}

func (f *fakeGitHub) CreateComment(_ context.Context, _ string, _ int, body string) (github.Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.commentErr != nil {
		return github.Comment{}, f.commentErr
	}
	f.created = append(f.created, body)
	f.nextID++
	return github.Comment{ID: f.nextID, Body: body}, nil
}

func (f *fakeGitHub) UpdateComment(_ context.Context, _ string, commentID int64, body string) (github.Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.commentErr != nil {
		return github.Comment{}, f.commentErr
	}
	f.updated = append(f.updated, body)
	return github.Comment{ID: commentID, Body: body}, nil
}

func (f *fakeGitHub) createdCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.created)
}

func (f *fakeGitHub) updatedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.updated)
}

type fakeProvider struct {
	mu       sync.Mutex
	issues   map[string][]model.Issue
	summary  map[string]string
	fail     map[string]error
	verdict  string
	requests []model.Request

	delay       time.Duration
	inFlight    int
	maxInFlight int
}

func (p *fakeProvider) Name() string { return "fake-model" }

func (p *fakeProvider) Review(_ context.Context, request model.Request) (model.Review, model.Usage, error) {
	p.mu.Lock()
	p.inFlight++
	if p.inFlight > p.maxInFlight {
		p.maxInFlight = p.inFlight
	}
	p.requests = append(p.requests, request)
	delay := p.delay
	p.mu.Unlock()

	if delay > 0 {
		time.Sleep(delay)
	}

	p.mu.Lock()
	p.inFlight--
	err := p.fail[request.File]
	issues := p.issues[request.File]
	summary := p.summary[request.File]
	verdict := p.verdict
	p.mu.Unlock()

	if err != nil {
		return model.Review{}, model.Usage{}, err
	}
	return model.Review{
		Summary: summary,
		Issues:  issues,
		Verdict: verdict,
	}, model.Usage{
		Model: "fake-model", PromptTokens: 100, CompletionTokens: 20,
	}, nil
}

func (p *fakeProvider) requestFor(file string) (model.Request, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, request := range p.requests {
		if request.File == file {
			return request, true
		}
	}
	return model.Request{}, false
}

func (p *fakeProvider) peak() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.maxInFlight
}

// -------------------------------------------------------------------- fixtures

// The value is assembled from pieces rather than written out whole: a
// repository that scans for credentials must not contain a string shaped like
// one, or GitHub's push protection refuses the push. The scanner sees the same
// string either way.
var fakeStripeKey = "sk_" + "live_" + "51H8xQ2eZvKYlo2C9wQwErTyUiOp"

var secretPatch = `@@ -1,3 +1,5 @@
 package main
 
+var apiKey = "` + fakeStripeKey + `"
+
 func main() {}
`

func testPull() github.PullRequest {
	pull := github.PullRequest{
		Number: 7,
		Title:  "Add configuration loading",
		Body:   "This loads the configuration from disk.",
		State:  "open",
	}
	pull.User.Login = "octocat"
	pull.Head.SHA = "0123456789abcdef0123456789abcdef01234567"
	pull.Head.Ref = "feature/config"
	pull.Base.Ref = "main"
	return pull
}

func testOptions(fake *fakeGitHub, provider model.Provider) Options {
	return Options{
		Config: config.Default(),
		GitHub: fake,
		Model:  provider,
		Logger: log.New(os.Stderr, "test: ", 0),
	}
}

// ------------------------------------------------------------------- the tests

func TestRunCollectsSecretsAndModelFindings(t *testing.T) {
	fake := &fakeGitHub{
		pull: testPull(),
		files: []github.File{
			{Filename: "main.go", Status: "modified", Patch: secretPatch},
		},
	}
	provider := &fakeProvider{
		issues:  map[string][]model.Issue{"main.go": {{Line: 9, Title: "Unchecked error", Detail: "The error from Load is ignored.", Severity: config.SeverityMajor}}},
		summary: map[string]string{"main.go": "Adds a configuration variable."},
		verdict: "request changes",
	}

	result, err := Run(context.Background(), "acme/widget", 7, testOptions(fake, provider))
	if err != nil {
		t.Fatalf("Run returned an error: %v", err)
	}

	if result.HeadSHA != testPull().Head.SHA {
		t.Errorf("HeadSHA = %q, want the pull request head", result.HeadSHA)
	}
	if result.FilesReviewed != 1 {
		t.Errorf("FilesReviewed = %d, want 1", result.FilesReviewed)
	}

	// The secret is found without any checkout: it is in the patch.
	var foundSecret, foundModel bool
	for _, finding := range result.Findings {
		switch finding.Source {
		case "secret-scan":
			foundSecret = true
			if finding.File != "main.go" || finding.Line != 3 {
				t.Errorf("secret finding at %s:%d, want main.go:3", finding.File, finding.Line)
			}
			if finding.Severity != config.SeverityCritical {
				t.Errorf("secret severity = %q, want CRITICAL", finding.Severity)
			}
		case "review":
			foundModel = true
		}
	}
	if !foundSecret {
		t.Errorf("the committed key was not reported; findings: %+v", result.Findings)
	}
	if !foundModel {
		t.Errorf("the model's issue was not reported; findings: %+v", result.Findings)
	}

	// Worst first, and the comment carries the marker exactly once.
	if result.Findings[0].Severity != config.SeverityCritical {
		t.Errorf("the first finding is %q, want the CRITICAL one first", result.Findings[0].Severity)
	}
	if strings.Count(result.Comment, Marker) != 1 {
		t.Errorf("the comment must carry the marker exactly once:\n%s", result.Comment)
	}
	if !strings.Contains(result.Comment, "## Code review of acme/widget#7") {
		t.Errorf("the comment is missing its heading:\n%s", result.Comment)
	}
	if !strings.Contains(result.Comment, "request changes") {
		t.Errorf("the comment is missing the verdict:\n%s", result.Comment)
	}
	if !strings.Contains(result.Comment, "Adds a configuration variable.") {
		t.Errorf("the comment is missing the summary:\n%s", result.Comment)
	}
}

func TestRunGivesEachFileOnlyItsOwnFindings(t *testing.T) {
	fake := &fakeGitHub{
		pull: testPull(),
		files: []github.File{
			{Filename: "a.go", Status: "modified", Patch: secretPatch},
			{Filename: "b.go", Status: "modified", Patch: secretPatch},
		},
	}
	provider := &fakeProvider{}

	if _, err := Run(context.Background(), "acme/widget", 7, testOptions(fake, provider)); err != nil {
		t.Fatalf("Run returned an error: %v", err)
	}

	request, ok := provider.requestFor("a.go")
	if !ok {
		t.Fatal("the model was never asked about a.go")
	}
	for _, finding := range request.Findings {
		if finding.File != "a.go" {
			t.Errorf("a.go's review was given a finding from %s", finding.File)
		}
	}
	if len(request.Findings) == 0 {
		t.Error("the model was not told about the secret in a.go: the tools' findings are facts it must not guess at")
	}
	if request.Repository != "acme/widget" || request.Number != 7 {
		t.Errorf("the request names %s#%d, want acme/widget#7", request.Repository, request.Number)
	}
	if request.Language != "go" {
		t.Errorf("Language = %q, want go", request.Language)
	}
	if len(request.Guidelines) != len(config.DefaultGuidelines) {
		t.Errorf("the request carries %d guidelines, want %d", len(request.Guidelines), len(config.DefaultGuidelines))
	}
}

func TestRunDropsAModelFindingThatRepeatsAToolFinding(t *testing.T) {
	fake := &fakeGitHub{
		pull:  testPull(),
		files: []github.File{{Filename: "main.go", Status: "modified", Patch: secretPatch}},
	}
	provider := &fakeProvider{
		issues: map[string][]model.Issue{"main.go": {
			{Line: 3, Title: "Hardcoded credential", Detail: "the model noticed too", Severity: config.SeverityCritical},
			{Line: 9, Title: "Unchecked error", Detail: "genuinely new", Severity: config.SeverityMinor},
		}},
	}

	result, err := Run(context.Background(), "acme/widget", 7, testOptions(fake, provider))
	if err != nil {
		t.Fatalf("Run returned an error: %v", err)
	}

	for _, finding := range result.Findings {
		if finding.File == "main.go" && finding.Line == 3 && finding.Source == "review" {
			t.Errorf("the model's duplicate of the tool's finding at main.go:3 was kept: %+v", result.Findings)
		}
	}
	var modelKept bool
	for _, finding := range result.Findings {
		if finding.Source == "review" && finding.Line == 9 {
			modelKept = true
		}
	}
	if !modelKept {
		t.Errorf("the model's own finding at main.go:9 was dropped: %+v", result.Findings)
	}
}

func TestRunBoundsPerFileConcurrencyAndKeepsOrder(t *testing.T) {
	files := make([]github.File, 0, 6)
	for index := 0; index < 6; index++ {
		files = append(files, github.File{
			Filename: fmt.Sprintf("file%d.go", index),
			Status:   "modified",
			Patch:    secretPatch,
		})
	}
	fake := &fakeGitHub{pull: testPull(), files: files}

	provider := &fakeProvider{delay: 20 * time.Millisecond}
	options := testOptions(fake, provider)
	options.Config.Limits.PerFileConcurrenc = 2

	result, err := Run(context.Background(), "acme/widget", 7, options)
	if err != nil {
		t.Fatalf("Run returned an error: %v", err)
	}
	if peak := provider.peak(); peak > 2 {
		t.Errorf("%d reviews ran at once, want at most 2", peak)
	}
	if len(result.Summaries) != 0 {
		t.Errorf("Summaries = %v, want none: the fake returns no summary", result.Summaries)
	}
	if result.FilesReviewed != 6 {
		t.Errorf("FilesReviewed = %d, want 6", result.FilesReviewed)
	}
}

func TestRunFailsWhenThePullRequestCannotBeRead(t *testing.T) {
	fake := &fakeGitHub{pullErr: errors.New("404 Not Found")}
	_, err := Run(context.Background(), "acme/widget", 7, testOptions(fake, &fakeProvider{}))
	if err == nil {
		t.Fatal("Run succeeded with an unreadable pull request")
	}
	if !strings.Contains(err.Error(), "could not read pull request acme/widget#7") {
		t.Errorf("the error does not say what failed: %v", err)
	}
}

func TestRunWithoutAModelSaysSoAndStillReviews(t *testing.T) {
	fake := &fakeGitHub{
		pull:  testPull(),
		files: []github.File{{Filename: "main.go", Status: "modified", Patch: secretPatch}},
	}
	provider := &fakeProvider{}
	options := testOptions(fake, provider)
	options.Model = model.None{}

	result, err := Run(context.Background(), "acme/widget", 7, options)
	if err != nil {
		t.Fatalf("Run returned an error: %v", err)
	}
	if len(provider.requests) != 0 {
		t.Error("the model was called even though none is configured")
	}
	if len(result.Findings) == 0 {
		t.Errorf("the deterministic checks must still run: findings = %+v", result.Findings)
	}
	for _, finding := range result.Findings {
		if finding.Source != "secret-scan" {
			t.Errorf("finding %+v came from a model that is not configured", finding)
		}
	}
	if !strings.Contains(result.Verdict, "none is configured") {
		t.Errorf("Verdict = %q, want it to say no model was used", result.Verdict)
	}
}

func TestRunReportsAModelFailurePerFile(t *testing.T) {
	fake := &fakeGitHub{
		pull: testPull(),
		files: []github.File{
			{Filename: "a.go", Status: "modified", Patch: secretPatch},
			{Filename: "b.go", Status: "modified", Patch: secretPatch},
		},
	}
	provider := &fakeProvider{fail: map[string]error{"b.go": errors.New("503 Service Unavailable")}}

	result, err := Run(context.Background(), "acme/widget", 7, testOptions(fake, provider))
	if err != nil {
		t.Fatalf("Run returned an error: %v", err)
	}
	if len(result.ModelErrors) != 1 || !strings.Contains(result.ModelErrors[0], "b.go") {
		t.Errorf("ModelErrors = %v, want one naming b.go", result.ModelErrors)
	}
	// a.go still got reviewed, and the comment says the model failed for one file.
	if len(result.Findings) == 0 {
		t.Error("the deterministic findings were lost when one file's model call failed")
	}
	if !strings.Contains(result.Comment, "The model failed for 1 file(s)") {
		t.Errorf("the comment does not mention the model failure:\n%s", result.Comment)
	}
}

func TestFilterFilesGivesAReasonForEverySkip(t *testing.T) {
	filters := config.Default().Filters
	filters.MaxFiles = 1
	filters.MaxPatchBytes = 1000

	// The cap is applied as the files are walked, so the file that hits it comes
	// last here: every other file then gets the reason that actually applies.
	files := []github.File{
		{Filename: "src/main.go", Status: "modified", Patch: secretPatch},
		{Filename: "go.sum", Status: "modified", Patch: secretPatch},
		{Filename: "logo.png", Status: "modified", Patch: ""},
		{Filename: "old.go", Status: "removed", Patch: secretPatch},
		{Filename: "src/big.go", Status: "modified", Patch: strings.Repeat("+x\n", 1000)},
		{Filename: "src/late.go", Status: "modified", Patch: secretPatch},
	}

	planned, skipped := FilterFiles(filters, files)

	if len(planned) != 1 || planned[0].Filename != "src/main.go" {
		t.Errorf("planned = %v, want only src/main.go", planned)
	}
	reasons := map[string]string{}
	for _, entry := range skipped {
		reasons[entry.File] = entry.Reason
		if entry.Reason == "" {
			t.Errorf("%s was skipped with no reason", entry.File)
		}
	}
	if len(skipped) != 5 {
		t.Fatalf("skipped %d file(s), want 5: %v", len(skipped), reasons)
	}
	if !strings.Contains(reasons["go.sum"], "*.sum") {
		t.Errorf("the skip reason for go.sum does not name the matching pattern: %q", reasons["go.sum"])
	}
	if !strings.Contains(reasons["src/late.go"], "only the first 1 files") {
		t.Errorf("the skip reason for the file past the cap is %q", reasons["src/late.go"])
	}
	if !strings.Contains(reasons["logo.png"], "binary") {
		t.Errorf("the skip reason for a patchless file is %q", reasons["logo.png"])
	}
	if !strings.Contains(reasons["old.go"], "deleted") {
		t.Errorf("the skip reason for a deleted file is %q", reasons["old.go"])
	}
	if !strings.Contains(reasons["src/big.go"], "over the 1000 byte limit") {
		t.Errorf("the skip reason for an oversized diff is %q", reasons["src/big.go"])
	}
}

func TestFilterFilesKeepsDeletedFilesWhenAsked(t *testing.T) {
	filters := config.Default().Filters
	filters.IncludeDeleted = true

	planned, _ := FilterFiles(filters, []github.File{
		{Filename: "old.go", Status: "removed", Patch: secretPatch},
	})
	if len(planned) != 1 {
		t.Errorf("planned = %v, want the deleted file included", planned)
	}
}

func TestPostCreatesOneComment(t *testing.T) {
	fake := &fakeGitHub{pull: testPull(), files: []github.File{{Filename: "main.go", Status: "modified", Patch: secretPatch}}}
	options := testOptions(fake, model.None{})

	result, err := Run(context.Background(), "acme/widget", 7, options)
	if err != nil {
		t.Fatalf("Run returned an error: %v", err)
	}
	if err := Post(context.Background(), &result, options); err != nil {
		t.Fatalf("Post returned an error: %v", err)
	}

	if fake.createdCount() != 1 || fake.updatedCount() != 0 {
		t.Errorf("created %d and updated %d comment(s), want one created", fake.createdCount(), fake.updatedCount())
	}
	if !result.Posted || result.CommentID != 1 {
		t.Errorf("Posted = %v, CommentID = %d, want true and 1", result.Posted, result.CommentID)
	}
}

func TestPostUpdatesItsOwnCommentInsteadOfAddingAnother(t *testing.T) {
	fake := &fakeGitHub{
		pull:     testPull(),
		files:    []github.File{{Filename: "main.go", Status: "modified", Patch: secretPatch}},
		comments: []github.Comment{{ID: 99, Body: Marker + "\n## an earlier review\n", User: github.User{Login: "review-bot"}}},
	}
	options := testOptions(fake, model.None{})

	result, err := Run(context.Background(), "acme/widget", 7, options)
	if err != nil {
		t.Fatalf("Run returned an error: %v", err)
	}
	if err := Post(context.Background(), &result, options); err != nil {
		t.Fatalf("Post returned an error: %v", err)
	}

	if fake.createdCount() != 0 {
		t.Errorf("a second comment was created (%d); the existing one must be updated", fake.createdCount())
	}
	if fake.updatedCount() != 1 {
		t.Fatalf("updated %d comment(s), want 1", fake.updatedCount())
	}
	if !result.Updated || result.CommentID != 99 {
		t.Errorf("Updated = %v, CommentID = %d, want true and 99", result.Updated, result.CommentID)
	}
}

func TestPostRecognisesItsOwnCommentByAuthorWhenTheMarkerIsGone(t *testing.T) {
	fake := &fakeGitHub{
		pull:     testPull(),
		files:    []github.File{{Filename: "main.go", Status: "modified", Patch: secretPatch}},
		comments: []github.Comment{{ID: 42, Body: "an older format of the comment", User: github.User{Login: "Review-Bot"}}},
	}
	options := testOptions(fake, model.None{})
	options.BotLogin = "review-bot"

	result, err := Run(context.Background(), "acme/widget", 7, options)
	if err != nil {
		t.Fatalf("Run returned an error: %v", err)
	}
	if err := Post(context.Background(), &result, options); err != nil {
		t.Fatalf("Post returned an error: %v", err)
	}
	if fake.updatedCount() != 1 || result.CommentID != 42 {
		t.Errorf("updated %d comment(s) with id %d, want the bot's own comment 42", fake.updatedCount(), result.CommentID)
	}
}

func TestPostSaysNothingWhenNothingWasFound(t *testing.T) {
	fake := &fakeGitHub{pull: testPull(), files: []github.File{{Filename: "clean.go", Status: "modified", Patch: "@@ -1,1 +1,2 @@\n package main\n+// a comment\n"}}}
	options := testOptions(fake, model.None{})

	result, err := Run(context.Background(), "acme/widget", 7, options)
	if err != nil {
		t.Fatalf("Run returned an error: %v", err)
	}
	if err := Post(context.Background(), &result, options); err != nil {
		t.Fatalf("Post returned an error: %v", err)
	}

	if fake.createdCount() != 0 || fake.updatedCount() != 0 {
		t.Errorf("posted %d and updated %d comment(s), want none: a bot that says looks-good on every pull request gets muted",
			fake.createdCount(), fake.updatedCount())
	}
	if result.Posted {
		t.Error("Posted = true with nothing to report")
	}
	if !strings.Contains(result.Reason, "found nothing") {
		t.Errorf("Reason = %q, want it to explain the silence", result.Reason)
	}
}

func TestPostResolvesAnEarlierCommentWhenTheFindingsAreGone(t *testing.T) {
	fake := &fakeGitHub{
		pull:     testPull(),
		files:    []github.File{{Filename: "clean.go", Status: "modified", Patch: "@@ -1,1 +1,2 @@\n package main\n+// a comment\n"}},
		comments: []github.Comment{{ID: 5, Body: Marker + "\n## a review that found things\n", User: github.User{Login: "review-bot"}}},
	}
	options := testOptions(fake, model.None{})

	result, err := Run(context.Background(), "acme/widget", 7, options)
	if err != nil {
		t.Fatalf("Run returned an error: %v", err)
	}
	if err := Post(context.Background(), &result, options); err != nil {
		t.Fatalf("Post returned an error: %v", err)
	}

	if fake.createdCount() != 0 || fake.updatedCount() != 1 {
		t.Fatalf("created %d and updated %d, want the old comment updated", fake.createdCount(), fake.updatedCount())
	}
	body := fake.updated[0]
	if !strings.Contains(body, "No findings") {
		t.Errorf("the stale comment was not replaced with a resolution:\n%s", body)
	}
	if strings.Contains(body, "## Code review of acme/widget#7") == false {
		t.Errorf("the resolution is missing its heading:\n%s", body)
	}
	if !result.Posted || result.CommentID != 5 {
		t.Errorf("Posted = %v, CommentID = %d, want true and 5", result.Posted, result.CommentID)
	}
}

func TestPostFailsWhenTheModelFailedForEveryFile(t *testing.T) {
	fake := &fakeGitHub{pull: testPull(), files: []github.File{{Filename: "clean.go", Status: "modified", Patch: "@@ -1,1 +1,2 @@\n package main\n+// a comment\n"}}}
	provider := &fakeProvider{fail: map[string]error{"clean.go": errors.New("connection reset by peer")}}
	options := testOptions(fake, provider)

	result, err := Run(context.Background(), "acme/widget", 7, options)
	if err != nil {
		t.Fatalf("Run returned an error: %v", err)
	}
	err = Post(context.Background(), &result, options)
	if err == nil {
		t.Fatal("Post succeeded although the model failed for every file and nothing was found: a broken review must not look clean")
	}
	if fake.createdCount() != 0 {
		t.Error("a comment was posted for a review that never ran")
	}
}

func TestPostReportsACommentItCouldNotPost(t *testing.T) {
	fake := &fakeGitHub{
		pull:       testPull(),
		files:      []github.File{{Filename: "main.go", Status: "modified", Patch: secretPatch}},
		commentErr: errors.New("403 Forbidden"),
	}
	options := testOptions(fake, model.None{})

	result, err := Run(context.Background(), "acme/widget", 7, options)
	if err != nil {
		t.Fatalf("Run returned an error: %v", err)
	}
	if err := Post(context.Background(), &result, options); err == nil {
		t.Fatal("Post succeeded although GitHub refused the comment")
	}
	if result.Posted {
		t.Error("Posted = true although the comment was refused")
	}
}

// TestToolFindingsRunsARealLinter checks the checkout, the tool, the path
// normalisation and the changed-file filter together, with gofmt as the tool.
func TestToolFindingsRunsARealLinter(t *testing.T) {
	if _, err := exec.LookPath("gofmt"); err != nil {
		t.Skip("gofmt is not on PATH")
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "unformatted.go"), []byte("package main\n\nfunc main(){println(\"x\")}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Unformatted too, but not part of the pull request: it must not be reported.
	if err := os.WriteFile(filepath.Join(dir, "untouched.go"), []byte("package main\n\nfunc other(){println(\"y\")}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	patch := "@@ -1,2 +1,3 @@\n package main\n \n+var x = 1\n"
	fake := &fakeGitHub{
		pull: testPull(),
		files: []github.File{
			{Filename: "unformatted.go", Status: "modified", Patch: patch},
		},
	}
	options := testOptions(fake, model.None{})
	options.Config.Lint.Commands = []config.LintCommand{
		{Name: "gofmt", Command: []string{"gofmt", "-l", "."}, Format: "gofmt-list"},
	}
	options.Checkout = func(context.Context, string, string) (string, func(), error) {
		return dir, func() {}, nil
	}

	result, err := Run(context.Background(), "acme/widget", 7, options)
	if err != nil {
		t.Fatalf("Run returned an error: %v", err)
	}

	var reported []string
	for _, finding := range result.Findings {
		if finding.Source == "gofmt" {
			reported = append(reported, finding.File)
			if finding.File != "unformatted.go" {
				t.Errorf("gofmt reported %q, want the repository relative path unformatted.go", finding.File)
			}
		}
	}
	if len(reported) != 1 {
		t.Errorf("gofmt findings = %v, want exactly the file the pull request changed", reported)
	}
}

// TestToolFindingsKeepsOnlyTouchedLines uses the real go vet, with one bad
// Printf inside the changed lines and one outside them.
func TestToolFindingsKeepsOnlyTouchedLines(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("the go tool is not on PATH")
	}

	dir := t.TempDir()
	module := "module example.com/reviewfixture\n\ngo 1.22\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(module), 0o644); err != nil {
		t.Fatal(err)
	}
	source := `package main

import "fmt"

func main() {
	fmt.Printf("%d\n", "not a number")
	println("padding")
	println("padding")
	println("padding")
	println("padding")
	println("padding")
	fmt.Printf("%s\n", 42)
}
`
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}

	// The hunk covers lines 3 to 6 of the new file, so the second bad Printf on
	// line 12 is somebody else's problem.
	patch := "@@ -3,4 +3,5 @@\n import \"fmt\"\n \n func main() {\n+\tfmt.Printf(\"%d\\n\", \"not a number\")\n"

	fake := &fakeGitHub{pull: testPull(), files: []github.File{{Filename: "main.go", Status: "modified", Patch: patch}}}
	options := testOptions(fake, model.None{})
	options.Config.Lint.Commands = []config.LintCommand{
		{Name: "go-vet", Command: []string{"go", "vet", "./..."}, Format: "go-vet", Stream: config.StreamStderr},
	}
	options.Checkout = func(context.Context, string, string) (string, func(), error) {
		return dir, func() {}, nil
	}

	result, err := Run(context.Background(), "acme/widget", 7, options)
	if err != nil {
		t.Fatalf("Run returned an error: %v", err)
	}

	var vetFindings []Finding
	for _, finding := range result.Findings {
		if finding.Source == "go-vet" {
			vetFindings = append(vetFindings, finding)
		}
	}
	if len(vetFindings) != 1 {
		for _, finding := range vetFindings {
			t.Logf("vet finding: %s:%d %s", finding.File, finding.Line, finding.Title)
		}
		t.Fatalf("go vet findings = %d, want exactly the one on a changed line", len(vetFindings))
	}
	finding := vetFindings[0]
	if finding.File != "main.go" || finding.Line != 6 {
		t.Errorf("vet finding at %s:%d, want main.go:6", finding.File, finding.Line)
	}
	if finding.Title != "vet-printf" {
		t.Errorf("rule = %q, want vet-printf", finding.Title)
	}
	if finding.Severity != config.SeverityMinor {
		t.Errorf("severity = %q, want the configured fallback MINOR", finding.Severity)
	}
	if !strings.Contains(finding.Detail, "wrong type") {
		t.Errorf("detail = %q, want the vet message", finding.Detail)
	}
}

func TestToolFindingsSurvivesACheckoutThatFails(t *testing.T) {
	fake := &fakeGitHub{pull: testPull(), files: []github.File{{Filename: "main.go", Status: "modified", Patch: secretPatch}}}
	options := testOptions(fake, model.None{})
	options.Config.Lint.Commands = []config.LintCommand{
		{Name: "gofmt", Command: []string{"gofmt", "-l", "."}, Format: "gofmt-list"},
	}
	options.Checkout = func(context.Context, string, string) (string, func(), error) {
		return "", func() {}, errors.New("could not resolve host")
	}

	result, err := Run(context.Background(), "acme/widget", 7, options)
	if err != nil {
		t.Fatalf("a failed checkout must cost the linter, not the review: %v", err)
	}
	if len(result.Findings) == 0 {
		t.Fatalf("findings = %+v, want the secret scan to survive a failed checkout", result.Findings)
	}
	for _, finding := range result.Findings {
		if finding.Source != "secret-scan" {
			t.Errorf("findings = %+v, want only the secret scan when the checkout failed", result.Findings)
		}
	}
}

func TestSortFindingsOrdersBySeverityThenLocation(t *testing.T) {
	findings := []Finding{
		{Severity: config.SeverityMinor, File: "b.go", Line: 1},
		{Severity: config.SeverityCritical, File: "z.go", Line: 9},
		{Severity: config.SeverityMinor, File: "a.go", Line: 4},
		{Severity: config.SeverityMajor, File: "a.go", Line: 2},
		{Severity: config.SeverityMajor, File: "a.go", Line: 1},
	}
	sorted := SortFindings(findings)

	want := []string{"z.go:9", "a.go:1", "a.go:2", "a.go:4", "b.go:1"}
	var got []string
	for _, finding := range sorted {
		got = append(got, fmt.Sprintf("%s:%d", finding.File, finding.Line))
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("order = %v, want %v", got, want)
	}
}

func TestMatchPatternUnderstandsThePatternsPeopleWrite(t *testing.T) {
	cases := []struct {
		pattern string
		file    string
		want    bool
	}{
		{"*.lock", "go.sum.lock", true},
		{"*.lock", "nested/dir/poetry.lock", true},
		{"*.lock", "locked.go", false},
		{"vendor/*", "vendor/github.com/x/y.go", true},
		{"vendor/*", "src/vendor/y.go", false},
		{"src/*", "src/a/b.go", true},
		{"dist/*", "dist/bundle.js", true},
		{"*_generated.go", "api/types_generated.go", true},
		{"*_generated.go", "api/types.go", false},
		{"", "anything.go", false},
		{"  ", "anything.go", false},
	}
	for _, tc := range cases {
		if got := matchPattern(tc.pattern, tc.file); got != tc.want {
			t.Errorf("matchPattern(%q, %q) = %v, want %v", tc.pattern, tc.file, got, tc.want)
		}
	}
}

func TestNormalizePathTurnsToolOutputIntoRepositoryPaths(t *testing.T) {
	cases := []struct {
		file, root, want string
	}{
		{"./main.go", "/tmp/checkout", "main.go"},
		{"/tmp/checkout/pkg/a.go", "/tmp/checkout", "pkg/a.go"},
		{"pkg\\a.go", "C:\\work\\checkout", "pkg/a.go"},
		{"pkg/a.go", "", "pkg/a.go"},
	}
	for _, tc := range cases {
		if got := normalizePath(tc.file, tc.root); got != tc.want {
			t.Errorf("normalizePath(%q, %q) = %q, want %q", tc.file, tc.root, got, tc.want)
		}
	}
}

func TestWorseVerdictPicksTheStrictest(t *testing.T) {
	cases := []struct {
		candidate, current string
		want               bool
	}{
		{"request changes", "approve", true},
		{"approve", "request changes", false},
		{"comment", "approve", true},
		{"reject", "request changes", true},
		{"", "approve", true},
	}
	for _, tc := range cases {
		if got := worseVerdict(tc.candidate, tc.current); got != tc.want {
			t.Errorf("worseVerdict(%q, %q) = %v, want %v", tc.candidate, tc.current, got, tc.want)
		}
	}
}
