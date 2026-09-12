package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/arclops/code-review-agent/internal/config"
	"github.com/arclops/code-review-agent/internal/model"
	"github.com/arclops/code-review-agent/internal/review"
	"github.com/arclops/code-review-agent/internal/runner"
	"github.com/arclops/code-review-agent/internal/webhook"
)

// ------------------------------------------------------------------- fakes

// fakeGitHub is a GitHub API with one pull request, one comment thread and no
// network in sight.
type fakeGitHub struct {
	mu sync.Mutex

	files    []map[string]any
	comments []map[string]any
	created  []string
	updated  []string
	nextID   int64

	pullRequests int
	filePages    int
	commentPages int
}

func newFakeGitHub(patch string) *fakeGitHub {
	return &fakeGitHub{
		files: []map[string]any{
			{
				"filename":  "config.go",
				"status":    "modified",
				"additions": 3,
				"deletions": 0,
				"changes":   3,
				"patch":     patch,
			},
		},
	}
}

func (f *fakeGitHub) handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/repos/acme/widget/pulls/12", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.pullRequests++
		f.mu.Unlock()
		writeJSON(w, 200, map[string]any{
			"number": 12,
			"title":  "Load the configuration from disk",
			"body":   "Adds a loader and a default.",
			"state":  "open",
			"user":   map[string]any{"login": "octocat", "type": "User"},
			"head":   map[string]any{"ref": "feature/config", "sha": "0123456789abcdef0123456789abcdef01234567"},
			"base":   map[string]any{"ref": "main", "sha": "fedcba9876543210fedcba9876543210fedcba98"},
		})
	})

	mux.HandleFunc("/repos/acme/widget/pulls/12/files", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			writeJSON(w, 401, map[string]any{"message": "Requires authentication"})
			return
		}
		f.mu.Lock()
		f.filePages++
		files := append([]map[string]any(nil), f.files...)
		f.mu.Unlock()
		writeJSON(w, 200, files)
	})

	mux.HandleFunc("/repos/acme/widget/issues/12/comments", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()

		if r.Method == http.MethodPost {
			var payload struct {
				Body string `json:"body"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			f.created = append(f.created, payload.Body)
			f.nextID++
			comment := map[string]any{
				"id":   f.nextID,
				"body": payload.Body,
				"user": map[string]any{"login": "review-bot", "type": "Bot"},
			}
			f.comments = append(f.comments, comment)
			writeJSON(w, 201, comment)
			return
		}

		f.commentPages++
		writeJSON(w, 200, append([]map[string]any(nil), f.comments...))
	})

	mux.HandleFunc("/repos/acme/widget/issues/comments/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()

		var payload struct {
			Body string `json:"body"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.updated = append(f.updated, payload.Body)
		writeJSON(w, 200, map[string]any{
			"id":   1,
			"body": payload.Body,
			"user": map[string]any{"login": "review-bot", "type": "Bot"},
		})
	})

	return mux
}

func (f *fakeGitHub) counts() (created, updated int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.created), len(f.updated)
}

func (f *fakeGitHub) commentBodies() ([]string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.created...), append([]string(nil), f.updated...)
}

// fakeModel answers the OpenAI chat completions API.
type fakeModel struct {
	mu       sync.Mutex
	calls    int
	requests []map[string]any
	reply    string
	fail     bool
}

func (f *fakeModel) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls++
		f.mu.Unlock()

		if r.Header.Get("Authorization") != "Bearer test-model-key" {
			writeJSON(w, 401, map[string]any{"error": map[string]any{"message": "bad key"}})
			return
		}
		body, _ := io.ReadAll(r.Body)
		var decoded map[string]any
		_ = json.Unmarshal(body, &decoded)
		f.mu.Lock()
		f.requests = append(f.requests, decoded)
		fail := f.fail
		reply := f.reply
		f.mu.Unlock()

		if fail {
			writeJSON(w, 500, map[string]any{"error": map[string]any{"message": "the model is down"}})
			return
		}
		writeJSON(w, 200, map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"role": "assistant", "content": reply}},
			},
			"usage": map[string]any{"prompt_tokens": 900, "completion_tokens": 120},
		})
	})
	return mux
}

func (f *fakeModel) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// The patch adds a live-looking key on line 3, which the secret scan must catch
// without any checkout.
//
// The value is assembled from pieces rather than written out whole: a repository
// that scans for credentials must not contain a string shaped like one, or
// GitHub's push protection refuses the push. The scanner sees the same string
// either way.
var fakeStripeKey = "sk_" + "live_" + "51H8xQ2eZvKYlo2C9wQwErTyUiOp"

var testPatch = `@@ -1,3 +1,5 @@
 package main
 
+var apiKey = "` + fakeStripeKey + `"
+
 func main() {}
`

const modelReply = `{
  "summary": "Adds a configuration loader.",
  "issues": [
    {"severity": "MAJOR", "file": "config.go", "line": 9, "title": "Unchecked error", "detail": "Load's error is dropped."}
  ],
  "suggestions": ["Return the error from Load instead of logging it."],
  "verdict": "request changes"
}`

func testConfig(githubURL, modelURL string) config.Config {
	cfg := config.Default()
	cfg.APIURL = githubURL
	cfg.GithubToken = "test-token"
	cfg.Checkout.Enabled = false
	cfg.Model.Provider = "openai"
	cfg.Model.BaseURL = modelURL + "/v1"
	cfg.Model.Name = "test-model"
	cfg.Model.APIKeyEnv = "TEST_MODEL_KEY"
	return cfg
}

// deliverOverHTTP posts a signed delivery to a real server over a real
// connection, and returns the status and the response body.
//
// A recorder served in-process does not cancel the request context when the
// handler returns; net/http does. That difference hid a bug where every run was
// cancelled by the very act of acknowledging the delivery, so the tests that
// matter go over the wire.
func deliverOverHTTP(t *testing.T, server *httptest.Server, event string, body []byte, secret string) (int, []byte) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, server.URL+"/webhook/github", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("could not build the delivery: %v", err)
	}
	request.Header.Set("X-GitHub-Event", event)
	request.Header.Set("X-Hub-Signature-256", webhook.Sign(secret, body))
	request.Header.Set("Content-Type", "application/json")

	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("the delivery failed: %v", err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("could not read the response: %v", err)
	}
	return response.StatusCode, payload
}

// deliverPullRequest sends a pull request event and decodes the run record it
// was answered with.
func deliverPullRequest(t *testing.T, server *httptest.Server, action, secret string) runner.Record {
	t.Helper()
	status, payload := deliverOverHTTP(t, server, "pull_request", pullRequestEvent(action), secret)
	if status != http.StatusAccepted {
		t.Fatalf("the delivery was answered %d: %s", status, payload)
	}
	var record runner.Record
	if err := json.Unmarshal(payload, &record); err != nil {
		t.Fatalf("the delivery response is not a run record: %v", err)
	}
	return record
}

func pullRequestEvent(action string) []byte {
	body, err := json.Marshal(map[string]any{
		"action":     action,
		"number":     12,
		"repository": map[string]any{"full_name": "acme/widget"},
		"pull_request": map[string]any{
			"number": 12,
			"draft":  false,
			"head":   map[string]any{"sha": "0123456789abcdef0123456789abcdef01234567"},
		},
	})
	if err != nil {
		panic(err)
	}
	return body
}

// ------------------------------------------------------------------ the tests

// TestWebhookReviewEndToEnd drives the whole service: a signed webhook delivery
// arrives, a review runs against a fake GitHub and a fake model, and one
// comment is posted. Nothing here is stubbed inside the service itself.
func TestWebhookReviewEndToEnd(t *testing.T) {
	const secret = "the-webhook-secret"
	t.Setenv("GITHUB_TOKEN", "test-token")
	t.Setenv("TEST_MODEL_KEY", "test-model-key")

	github := newFakeGitHub(testPatch)
	githubServer := httptest.NewServer(github.handler())
	defer githubServer.Close()

	model := &fakeModel{reply: modelReply}
	modelServer := httptest.NewServer(model.handler())
	defer modelServer.Close()

	cfg := testConfig(githubServer.URL, modelServer.URL)
	built, err := buildDeps(cfg)
	if err != nil {
		t.Fatalf("buildDeps: %v", err)
	}
	logger := log.New(os.Stderr, "e2e: ", 0)

	queue := runner.New(runner.Options{
		Reviewer: reviewerFor(cfg, built, logger),
		Logger:   logger,
	})
	handler := webhook.New(webhook.Options{Secret: secret, Queue: queue, Logger: logger})
	server := httptest.NewServer(handler.Routes())
	defer server.Close()

	queued := deliverPullRequest(t, server, "opened", secret)
	queue.Wait()

	record, ok := queue.Get(queued.ID)
	if !ok {
		t.Fatal("the run disappeared from the history")
	}
	if record.State != runner.StateSucceeded {
		t.Fatalf("run %s is %q: %s", record.ID, record.State, record.Error)
	}

	created, updated := github.commentBodies()
	if len(created) != 1 || len(updated) != 0 {
		t.Fatalf("created %d and updated %d comment(s), want exactly one created", len(created), len(updated))
	}
	comment := created[0]

	// The tools' finding and the model's finding are both in the one comment.
	if !strings.Contains(comment, "stripe-secret-key") {
		t.Errorf("the committed key was not reported:\n%s", comment)
	}
	if !strings.Contains(comment, "Unchecked error") {
		t.Errorf("the model's finding was not reported:\n%s", comment)
	}
	if !strings.Contains(comment, "config.go:9") {
		t.Errorf("the model's finding has no location:\n%s", comment)
	}
	if !strings.Contains(comment, "CRITICAL") || !strings.Contains(comment, "MAJOR") {
		t.Errorf("the comment does not group the findings by severity:\n%s", comment)
	}
	if strings.Index(comment, "CRITICAL") > strings.Index(comment, "MAJOR") {
		t.Errorf("MAJOR appears before CRITICAL:\n%s", comment)
	}
	if strings.Count(comment, "<!-- code-review-agent -->") != 1 {
		t.Errorf("the marker is missing or repeated:\n%s", comment)
	}
	// The raw secret is never copied into a public comment.
	if strings.Contains(comment, fakeStripeKey) {
		t.Errorf("the comment echoes the credential it is reporting:\n%s", comment)
	}

	if model.callCount() != 1 {
		t.Errorf("the model was called %d time(s), want once, one per changed file", model.callCount())
	}
	if record.CommentID == 0 || !record.Posted {
		t.Errorf("the run record does not point at the comment it posted: %+v", record)
	}
}

// TestWebhookReviewUpdatesOneCommentAcrossPushes is the "three pushes, one
// comment" requirement: the second review edits the first comment.
func TestWebhookReviewUpdatesOneCommentAcrossPushes(t *testing.T) {
	const secret = "the-webhook-secret"
	t.Setenv("GITHUB_TOKEN", "test-token")
	t.Setenv("TEST_MODEL_KEY", "test-model-key")

	github := newFakeGitHub(testPatch)
	githubServer := httptest.NewServer(github.handler())
	defer githubServer.Close()

	model := &fakeModel{reply: modelReply}
	modelServer := httptest.NewServer(model.handler())
	defer modelServer.Close()

	cfg := testConfig(githubServer.URL, modelServer.URL)
	built, err := buildDeps(cfg)
	if err != nil {
		t.Fatalf("buildDeps: %v", err)
	}
	logger := log.New(os.Stderr, "e2e: ", 0)
	queue := runner.New(runner.Options{Reviewer: reviewerFor(cfg, built, logger), Logger: logger})
	handler := webhook.New(webhook.Options{Secret: secret, Queue: queue, Logger: logger})
	server := httptest.NewServer(handler.Routes())
	defer server.Close()

	for _, action := range []string{"opened", "synchronize", "synchronize"} {
		deliverPullRequest(t, server, action, secret)
		queue.Wait()
	}

	created, updated := github.counts()
	if created != 1 {
		t.Errorf("created %d comment(s), want 1: a review per push would bury the pull request", created)
	}
	if updated != 2 {
		t.Errorf("updated %d comment(s), want 2: every push after the first rewrites the same comment", updated)
	}
}

// TestQuietReviewLeavesNoComment checks the other half of the comment policy:
// a pull request with nothing wrong with it gets silence, not a green tick that
// everybody learns to ignore.
func TestQuietReviewLeavesNoComment(t *testing.T) {
	const secret = "the-webhook-secret"
	t.Setenv("GITHUB_TOKEN", "test-token")
	t.Setenv("TEST_MODEL_KEY", "test-model-key")

	github := newFakeGitHub("@@ -1,2 +1,3 @@\n package main\n \n+// A comment, and nothing else.\n")
	githubServer := httptest.NewServer(github.handler())
	defer githubServer.Close()

	model := &fakeModel{reply: `{"summary":"A comment.","issues":[],"suggestions":[],"verdict":"approve"}`}
	modelServer := httptest.NewServer(model.handler())
	defer modelServer.Close()

	cfg := testConfig(githubServer.URL, modelServer.URL)
	built, err := buildDeps(cfg)
	if err != nil {
		t.Fatalf("buildDeps: %v", err)
	}
	logger := log.New(os.Stderr, "e2e: ", 0)
	queue := runner.New(runner.Options{Reviewer: reviewerFor(cfg, built, logger), Logger: logger})
	handler := webhook.New(webhook.Options{Secret: secret, Queue: queue, Logger: logger})
	server := httptest.NewServer(handler.Routes())
	defer server.Close()

	queued := deliverPullRequest(t, server, "opened", secret)
	queue.Wait()

	record, _ := queue.Get(queued.ID)
	if record.State != runner.StateSkipped {
		t.Errorf("State = %q, want %q for a clean pull request", record.State, runner.StateSkipped)
	}
	if created, updated := github.counts(); created != 0 || updated != 0 {
		t.Errorf("created %d and updated %d comment(s), want silence", created, updated)
	}
}

// TestBrokenReviewIsRecordedAsFailed is the requirement that a review nobody
// could run must not look like a review that found nothing.
func TestBrokenReviewIsRecordedAsFailed(t *testing.T) {
	const secret = "the-webhook-secret"
	t.Setenv("GITHUB_TOKEN", "test-token")
	t.Setenv("TEST_MODEL_KEY", "test-model-key")

	github := newFakeGitHub("@@ -1,2 +1,3 @@\n package main\n \n+// Nothing wrong here.\n")
	githubServer := httptest.NewServer(github.handler())
	defer githubServer.Close()

	model := &fakeModel{fail: true}
	modelServer := httptest.NewServer(model.handler())
	defer modelServer.Close()

	cfg := testConfig(githubServer.URL, modelServer.URL)
	cfg.Limits.Retries = 2
	cfg.Limits.RetryBaseDelay = config.Seconds(1) // a nanosecond, so the retries do not slow the test down
	built, err := buildDeps(cfg)
	if err != nil {
		t.Fatalf("buildDeps: %v", err)
	}
	logger := log.New(os.Stderr, "e2e: ", 0)

	var reported []runner.Record
	queue := runner.New(runner.Options{
		Reviewer:  reviewerFor(cfg, built, logger),
		Logger:    logger,
		OnFailure: func(record runner.Record) { reported = append(reported, record) },
	})
	handler := webhook.New(webhook.Options{Secret: secret, Queue: queue, Logger: logger})
	server := httptest.NewServer(handler.Routes())
	defer server.Close()

	queued := deliverPullRequest(t, server, "opened", secret)
	queue.Wait()

	record, _ := queue.Get(queued.ID)
	if record.State != runner.StateFailed {
		t.Fatalf("State = %q, want %q: the model was down, so nothing was reviewed", record.State, runner.StateFailed)
	}
	if record.Error == "" {
		t.Error("the failure has no message")
	}
	if created, _ := github.counts(); created != 0 {
		t.Error("a comment was posted for a review that never ran")
	}
	if len(reported) != 1 {
		t.Fatalf("%d failure notification(s), want 1", len(reported))
	}
	if reported[0].ID != record.ID {
		t.Errorf("the notification names run %q, want %q", reported[0].ID, record.ID)
	}
	if model.callCount() < 2 {
		t.Errorf("the model was called %d time(s): a 500 must be retried", model.callCount())
	}
}

// ---------------------------------------------------------------------- the CLI

func TestReviewCommandPrintsTheReviewAndPostsNothing(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token")
	t.Setenv("TEST_MODEL_KEY", "")

	github := newFakeGitHub(testPatch)
	githubServer := httptest.NewServer(github.handler())
	defer githubServer.Close()
	t.Setenv("GITHUB_API_URL", githubServer.URL)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"review", "-repo", "acme/widget", "-pr", "12"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code %d, stderr: %s", code, stderr.String())
	}

	output := stdout.String()
	if !strings.Contains(output, "acme/widget#12") {
		t.Errorf("the output does not name the pull request:\n%s", output)
	}
	if !strings.Contains(output, "stripe-secret-key") {
		t.Errorf("the output does not report the committed key:\n%s", output)
	}
	if !strings.Contains(output, "<!-- code-review-agent -->") {
		t.Errorf("the review comment is not printed:\n%s", output)
	}
	if !strings.Contains(output, "nothing sent to GitHub") {
		t.Errorf("the output does not say that nothing was posted:\n%s", output)
	}
	if created, _ := github.counts(); created != 0 {
		t.Errorf("%d comment(s) were posted without -post", created)
	}
}

func TestReviewCommandPostsWhenAsked(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token")
	t.Setenv("TEST_MODEL_KEY", "")

	github := newFakeGitHub(testPatch)
	githubServer := httptest.NewServer(github.handler())
	defer githubServer.Close()
	t.Setenv("GITHUB_API_URL", githubServer.URL)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"review", "-repo", "acme/widget", "-pr", "12", "-post"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code %d, stderr: %s", code, stderr.String())
	}
	created, _ := github.counts()
	if created != 1 {
		t.Fatalf("posted %d comment(s), want 1", created)
	}
	if !strings.Contains(stdout.String(), "posted comment 1") {
		t.Errorf("the output does not say what was posted:\n%s", stdout.String())
	}
}

func TestReviewCommandRejectsBadArguments(t *testing.T) {
	cases := [][]string{
		{"review", "-repo", "not-a-repo", "-pr", "12"},
		{"review", "-repo", "acme/widget", "-pr", "0"},
		{"review", "-repo", "acme/widget"},
	}
	for _, args := range cases {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 2 {
			t.Errorf("%v exited %d, want 2", args, code)
		}
		if stderr.Len() == 0 {
			t.Errorf("%v said nothing about what was wrong", args)
		}
	}
}

func TestReviewCommandNeedsAToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")

	var stdout, stderr bytes.Buffer
	code := run([]string{"review", "-repo", "acme/widget", "-pr", "12"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code %d, want 1: a reviewer that cannot authenticate must refuse to start", code)
	}
	if !strings.Contains(stderr.String(), "GITHUB_TOKEN") {
		t.Errorf("the error does not name the missing variable: %s", stderr.String())
	}
}

func TestVersionAndHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("version exited %d", code)
	}
	if !strings.Contains(stdout.String(), version) {
		t.Errorf("version printed %q, want it to contain %q", stdout.String(), version)
	}

	stdout.Reset()
	if code := run([]string{"help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("help exited %d", code)
	}
	for _, command := range []string{"serve", "review", "runs"} {
		if !strings.Contains(stdout.String(), command) {
			t.Errorf("the help does not mention %q:\n%s", command, stdout.String())
		}
	}

	stdout.Reset()
	if code := run([]string{"frobnicate"}, &stdout, &stderr); code != 2 {
		t.Errorf("an unknown command exited %d, want 2", code)
	}
	if code := run(nil, io.Discard, io.Discard); code != 2 {
		t.Errorf("no arguments exited %d, want 2", code)
	}
}

func TestRunsCommandReadsTheHistoryFromAServer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/runs" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeJSON(w, 200, map[string]any{
			"runs": []map[string]any{
				{
					"id": "abc123", "repository": "acme/widget", "number": 12,
					"state": "failed", "error": "the model is down", "queued_at": "2026-01-01T00:00:00Z",
				},
				{
					"id": "def456", "repository": "acme/widget", "number": 12,
					"state": "succeeded", "findings": 2, "posted": true, "queued_at": "2026-01-01T00:01:00Z",
				},
			},
		})
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	if code := run([]string{"runs", "-server", server.URL}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code %d, stderr: %s", code, stderr.String())
	}
	output := stdout.String()
	if !strings.Contains(output, "abc123") || !strings.Contains(output, "failed") {
		t.Errorf("the failed run is missing from the history:\n%s", output)
	}
	if !strings.Contains(output, "the model is down") {
		t.Errorf("the failure reason is missing from the history:\n%s", output)
	}
	if !strings.Contains(output, "acme/widget#12") {
		t.Errorf("the pull request is missing from the history:\n%s", output)
	}
}

func TestRunsCommandReportsAServerItCannotReach(t *testing.T) {
	t.Setenv("REVIEW_SERVER", "http://127.0.0.1:1")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"runs"}, &stdout, &stderr); code != 1 {
		t.Errorf("exit code %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "could not reach") {
		t.Errorf("the error does not say what happened: %s", stderr.String())
	}
}

func TestLoadConfigRejectsSomethingThatCannotWork(t *testing.T) {
	path := t.TempDir() + string(os.PathSeparator) + "review.json"
	if err := os.WriteFile(path, []byte(`{"model":{"provider":"gpt"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"review", "-config", path, "-repo", "acme/widget", "-pr", "1"}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit code %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "unknown model provider") {
		t.Errorf("the error does not explain the problem: %s", stderr.String())
	}
}

func TestUnknownFlagIsRejected(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"review", "-nope"}, &stdout, &stderr); code != 2 {
		t.Errorf("exit code %d, want 2", code)
	}
}

// TestShippedExampleConfigIsValid keeps the documented example honest: a
// configuration file that does not load is worse than none.
func TestShippedExampleConfigIsValid(t *testing.T) {
	cfg, err := config.Load("examples/review.json")
	if err != nil {
		t.Fatalf("examples/review.json does not load: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("examples/review.json does not validate: %v", err)
	}
	if cfg.Model.Provider == "none" {
		t.Error("the example configures no model, which is not what it is an example of")
	}
	if len(cfg.Lint.Commands) == 0 {
		t.Error("the example configures no linters")
	}
	if len(cfg.Guidelines) == 0 {
		t.Error("the example configures no guidelines")
	}
	// Round tripping the configuration must not lose the sub-second retry
	// delay, which is what the service backs off by.
	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("the configuration does not marshal: %v", err)
	}
	var reloaded config.Config
	if err := json.Unmarshal(encoded, &reloaded); err != nil {
		t.Fatalf("the configuration does not unmarshal: %v", err)
	}
	if reloaded.Limits.RetryBaseDelay != cfg.Limits.RetryBaseDelay {
		t.Errorf("the retry delay became %v, want %v",
			reloaded.Limits.RetryBaseDelay.Duration(), cfg.Limits.RetryBaseDelay.Duration())
	}
}

func TestTruncateAndShortSHA(t *testing.T) {
	if got := shortSHA("0123456789abcdef"); got != "0123456" {
		t.Errorf("shortSHA = %q, want 0123456", got)
	}
	if got := shortSHA("abc"); got != "abc" {
		t.Errorf("shortSHA = %q, want abc", got)
	}
	if got := truncate("abcdefghij", 5); got != "ab..." {
		t.Errorf("truncate = %q, want ab...", got)
	}
	if got := truncate("abc", 5); got != "abc" {
		t.Errorf("truncate = %q, want abc", got)
	}
}

func TestEnvOrFallsBack(t *testing.T) {
	t.Setenv("REVIEW_TEST_VAR", "")
	if got := envOr("REVIEW_TEST_VAR", "fallback"); got != "fallback" {
		t.Errorf("envOr = %q, want fallback", got)
	}
	t.Setenv("REVIEW_TEST_VAR", "set")
	if got := envOr("REVIEW_TEST_VAR", "fallback"); got != "set" {
		t.Errorf("envOr = %q, want set", got)
	}
}

func TestBuildDepsRefusesToStartWithoutAToken(t *testing.T) {
	cfg := config.Default()
	if _, err := buildDeps(cfg); err == nil {
		t.Fatal("buildDeps succeeded with no token")
	} else if !strings.Contains(err.Error(), "GITHUB_TOKEN") {
		t.Errorf("the error does not name the variable to set: %v", err)
	}
}

func TestPrintResultListsEverythingItDid(t *testing.T) {
	result := review.Result{
		Repository:    "acme/widget",
		Number:        12,
		HeadSHA:       "0123456789abcdef",
		FilesReviewed: 2,
		FilesSkipped:  []review.Skipped{{File: "go.sum", Reason: "generated, vendored or locked: matches *.sum"}},
		Findings:      []review.Finding{{Severity: config.SeverityCritical, File: "config.go", Line: 3, Title: "stripe-secret-key", Source: "secret-scan"}},
		ModelErrors:   []string{"other.go: the model is down"},
		Usage:         []model.Usage{{Model: "test-model", PromptTokens: 10, CompletionTokens: 2}},
		Comment:       "<!-- code-review-agent -->\nbody\n",
		Reason:        "the review found something",
	}
	var out bytes.Buffer
	printResult(&out, result, false)

	for _, want := range []string{"acme/widget#12", "0123456", "2 file(s) reviewed", "skipped go.sum", "stripe-secret-key", "model error", "prompt", "nothing sent to GitHub", "<!-- code-review-agent -->"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the printed review is missing %q:\n%s", want, out.String())
		}
	}

	out.Reset()
	result.Posted = true
	result.Updated = true
	result.CommentID = 44
	printResult(&out, result, true)
	if !strings.Contains(out.String(), "updated comment 44") {
		t.Errorf("the output does not say the comment was updated:\n%s", out.String())
	}
}
