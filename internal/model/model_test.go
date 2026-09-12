package model

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arclops/code-review-agent/internal/config"
	"github.com/arclops/code-review-agent/internal/httpx"
)

var (
	_ Provider = None{}
	_ Provider = (*OpenAI)(nil)
	_ Provider = (*Anthropic)(nil)
)

// sleepLog records the waits a provider's policy asked for, so a retry test
// never actually waits.
type sleepLog struct {
	mutex sync.Mutex
	waits []time.Duration
}

func (l *sleepLog) sleep(context.Context, time.Duration) error {
	l.mutex.Lock()
	defer l.mutex.Unlock()
	l.waits = append(l.waits, 1)
	return nil
}

func (l *sleepLog) count() int {
	l.mutex.Lock()
	defer l.mutex.Unlock()
	return len(l.waits)
}

func testPolicy(attempts int) (httpx.Policy, *sleepLog) {
	log := &sleepLog{}
	return httpx.Policy{Attempts: attempts, Sleep: log.sleep}, log
}

// requestCapture keeps the last request a fake provider received.
type requestCapture struct {
	mutex  sync.Mutex
	method string
	path   string
	header http.Header
	body   []byte
}

func (c *requestCapture) record(request *http.Request) {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		body = []byte("could not read the body: " + err.Error())
	}
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.method = request.Method
	c.path = request.URL.Path
	c.header = request.Header.Clone()
	c.body = body
}

func (c *requestCapture) snapshot() (method, path string, header http.Header, body []byte) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.method, c.path, c.header, append([]byte(nil), c.body...)
}

func replyWith(t *testing.T, payload any) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(payload); err != nil {
			t.Errorf("encoding the fake reply: %v", err)
		}
	}
}

func openAITestRequest() Request {
	return Request{
		Repository:  "acme/widget",
		Number:      7,
		Title:       "Add retry logic",
		Description: "Retries the flaky call.",
		Author:      "dev",
		File:        "internal/httpx/httpx.go",
		Language:    "Go",
		Patch:       "@@ -1,1 +1,2 @@\n-old\n+new\n",
		Findings: []Finding{
			{File: "internal/httpx/httpx.go", Line: 12, Rule: "S105", Message: "possible hardcoded password", Severity: "CRITICAL", Tool: "ruff"},
		},
		Guidelines: []string{"Never commit secrets.", "Prefer early returns."},
	}
}

func TestExtractJSONFindsTheObjectInsideProse(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
		ok   bool
	}{
		{
			name: "object surrounded by prose",
			text: "Sure, here is the review: {\"summary\":\"ok\"} -- hope that helps.",
			want: `{"summary":"ok"}`,
			ok:   true,
		},
		{
			name: "nested objects",
			text: `{"a":{"b":{"c":1}},"d":[{"e":2}]}`,
			want: `{"a":{"b":{"c":1}},"d":[{"e":2}]}`,
			ok:   true,
		},
		{
			name: "a closing brace inside a string",
			text: `prefix {"summary":"closes } here","n":1} suffix`,
			want: `{"summary":"closes } here","n":1}`,
			ok:   true,
		},
		{
			name: "an opening brace inside a string",
			text: `{"summary":"opens { here","n":1}`,
			want: `{"summary":"opens { here","n":1}`,
			ok:   true,
		},
		{
			name: "an escaped quote inside a string",
			text: `{"summary":"he said \"hi\" and }","n":2}`,
			want: `{"summary":"he said \"hi\" and }","n":2}`,
			ok:   true,
		},
		{
			name: "an escaped backslash before a closing quote",
			text: `{"p":"C:\\tmp","n":1}`,
			want: `{"p":"C:\\tmp","n":1}`,
			ok:   true,
		},
		{
			name: "only the first object",
			text: `{"a":1} and then {"b":2}`,
			want: `{"a":1}`,
			ok:   true,
		},
		{
			name: "an object inside a code fence",
			text: "```json\n{\"summary\":\"ok\"}\n```",
			want: `{"summary":"ok"}`,
			ok:   true,
		},
		{
			name: "no JSON at all",
			text: "I could not review this file.",
			want: "",
			ok:   false,
		},
		{
			name: "truncated JSON",
			text: `{"summary":"ok"`,
			want: "",
			ok:   false,
		},
		{
			name: "an unbalanced close only",
			text: `} nothing {`,
			want: "",
			ok:   false,
		},
		{
			name: "an empty string",
			text: "",
			want: "",
			ok:   false,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, ok := ExtractJSON(testCase.text)
			if ok != testCase.ok {
				t.Fatalf("ExtractJSON(%q) ok = %v, want %v", testCase.text, ok, testCase.ok)
			}
			if got != testCase.want {
				t.Errorf("ExtractJSON(%q) = %q, want %q", testCase.text, got, testCase.want)
			}
		})
	}
}

// TestExtractJSONPrefersTheAnswerOverAMentionOfTheSchema covers the reply that
// breaks the naive scanner: the model explains the schema before filling it in,
// and the empty object in the explanation is not the review.
func TestExtractJSONPrefersTheAnswerOverAMentionOfTheSchema(t *testing.T) {
	const reply = "Return {} when there is nothing to say: {\"summary\":\"ok\"}"

	got, ok := ExtractJSON(reply)
	if !ok {
		t.Fatal("ExtractJSON() ok = false, want true")
	}
	if got != `{"summary":"ok"}` {
		t.Errorf("ExtractJSON() = %q, want the object the model meant", got)
	}

	review, err := ParseReview(reply)
	if err != nil {
		t.Fatalf("ParseReview() = %v, want nil", err)
	}
	if review.Summary != "ok" {
		t.Errorf("Summary = %q, want %q: an empty review would look like a clean pull request", review.Summary, "ok")
	}
}

// TestExtractJSONFallsBackToAnyValidObject covers a model that answers with a
// shape the prompt did not ask for: the object is still better than nothing.
func TestExtractJSONFallsBackToAnyValidObject(t *testing.T) {
	got, ok := ExtractJSON("here you go: {\"note\":\"nothing to report\"}")
	if !ok {
		t.Fatal("ExtractJSON() ok = false, want true")
	}
	if got != `{"note":"nothing to report"}` {
		t.Errorf("ExtractJSON() = %q, want the only object in the text", got)
	}
}

func TestParseReviewNormalisesWhatTheModelReturned(t *testing.T) {
	cases := []struct {
		name  string
		reply string
		check func(t *testing.T, review Review)
	}{
		{
			name:  "an empty object",
			reply: `{}`,
			check: func(t *testing.T, review Review) {
				if review.Summary != "" || review.Verdict != "" {
					t.Errorf("review = %+v, want zero values", review)
				}
				if len(review.Issues) != 0 || len(review.Suggestions) != 0 {
					t.Errorf("review = %+v, want no issues and no suggestions", review)
				}
			},
		},
		{
			name:  "null arrays",
			reply: `{"summary":"fine","issues":null,"suggestions":null,"verdict":"approve"}`,
			check: func(t *testing.T, review Review) {
				if review.Summary != "fine" || review.Verdict != "approve" {
					t.Errorf("review = %+v, want the summary and verdict", review)
				}
				if len(review.Issues) != 0 || len(review.Suggestions) != 0 {
					t.Errorf("review = %+v, want null arrays to become empty", review)
				}
			},
		},
		{
			name:  "text surrounded by whitespace",
			reply: `{"summary":"  spaced  ","verdict":"  approve  ","issues":[{"severity":"major","title":"  t  ","detail":"  d  "}]}`,
			check: func(t *testing.T, review Review) {
				if review.Summary != "spaced" {
					t.Errorf("Summary = %q, want it trimmed", review.Summary)
				}
				if review.Verdict != "approve" {
					t.Errorf("Verdict = %q, want it trimmed", review.Verdict)
				}
				if review.Issues[0].Title != "t" || review.Issues[0].Detail != "d" {
					t.Errorf("issue = %+v, want the title and detail trimmed", review.Issues[0])
				}
			},
		},
		{
			name:  "an issue with nothing to say is dropped",
			reply: `{"issues":[{"severity":"critical","title":"   ","detail":"  "},{"severity":"major","title":"keep","detail":"d"}]}`,
			check: func(t *testing.T, review Review) {
				if len(review.Issues) != 1 {
					t.Fatalf("issues = %+v, want only the issue that says something", review.Issues)
				}
				if review.Issues[0].Title != "keep" {
					t.Errorf("issues = %+v, want the issue with a title kept", review.Issues)
				}
			},
		},
		{
			name:  "a missing title comes from the first line of the detail",
			reply: `{"issues":[{"severity":"major","detail":"first line\nsecond line"}]}`,
			check: func(t *testing.T, review Review) {
				if len(review.Issues) != 1 {
					t.Fatalf("issues = %+v, want one issue", review.Issues)
				}
				if review.Issues[0].Title != "first line" {
					t.Errorf("Title = %q, want the first line of the detail", review.Issues[0].Title)
				}
				if review.Issues[0].Detail != "first line\nsecond line" {
					t.Errorf("Detail = %q, want the whole detail kept", review.Issues[0].Detail)
				}
			},
		},
		{
			name:  "suggestions are trimmed and blanks dropped",
			reply: `{"suggestions":["  keep me  ","","   ","also keep"]}`,
			check: func(t *testing.T, review Review) {
				want := []string{"keep me", "also keep"}
				if len(review.Suggestions) != len(want) {
					t.Fatalf("suggestions = %v, want %v", review.Suggestions, want)
				}
				for index := range want {
					if review.Suggestions[index] != want[index] {
						t.Errorf("suggestions = %v, want %v", review.Suggestions, want)
					}
				}
			},
		},
		{
			name:  "braces inside a string value",
			reply: `{"summary":"use { and } freely","verdict":"approve"}`,
			check: func(t *testing.T, review Review) {
				if review.Summary != "use { and } freely" {
					t.Errorf("Summary = %q, want the whole string kept", review.Summary)
				}
			},
		},
		{
			name:  "unknown fields are ignored",
			reply: `{"summary":"s","confidence":0.9,"issues":[{"severity":"high","title":"t","detail":"d","line":3,"extra":true}],"meta":{"model":"x"}}`,
			check: func(t *testing.T, review Review) {
				if review.Summary != "s" || len(review.Issues) != 1 {
					t.Errorf("review = %+v, want only the known fields", review)
				}
				if review.Issues[0].Line != 3 {
					t.Errorf("Line = %d, want 3", review.Issues[0].Line)
				}
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			review, err := ParseReview(testCase.reply)
			if err != nil {
				t.Fatalf("ParseReview() = %v, want nil", err)
			}
			testCase.check(t, review)
		})
	}
}

func TestParseReviewNormalisesEverySeveritySpelling(t *testing.T) {
	cases := []struct {
		written string
		want    string
	}{
		{written: "critical", want: config.SeverityCritical},
		{written: "CRITICAL", want: config.SeverityCritical},
		{written: " Critical ", want: config.SeverityCritical},
		{written: "Blocker", want: config.SeverityCritical},
		{written: "HIGH", want: config.SeverityCritical},
		{written: "Error", want: config.SeverityCritical},
		{written: "major", want: config.SeverityMajor},
		{written: "MAJOR", want: config.SeverityMajor},
		{written: "Medium", want: config.SeverityMajor},
		{written: "warning", want: config.SeverityMajor},
		{written: "warn", want: config.SeverityMajor},
		{written: "minor", want: config.SeverityMinor},
		{written: "LOW", want: config.SeverityMinor},
		{written: "info", want: config.SeverityMinor},
		{written: "nit", want: config.SeverityMinor},
		{written: "suggestion", want: config.SeverityMinor},
		{written: "", want: config.SeverityMinor},
		{written: "catastrophic", want: config.SeverityMinor},
		{written: "5", want: config.SeverityMinor},
	}
	for _, testCase := range cases {
		name := testCase.written
		if name == "" {
			name = "missing"
		}
		t.Run(name, func(t *testing.T) {
			reply := fmt.Sprintf(`{"issues":[{"severity":%q,"title":"t","detail":"d"}]}`, testCase.written)
			review, err := ParseReview(reply)
			if err != nil {
				t.Fatalf("ParseReview() = %v, want nil", err)
			}
			if len(review.Issues) != 1 {
				t.Fatalf("issues = %+v, want one issue", review.Issues)
			}
			if review.Issues[0].Severity != testCase.want {
				t.Errorf("severity %q became %q, want %q", testCase.written, review.Issues[0].Severity, testCase.want)
			}
		})
	}
}

func TestParseReviewRejectsRepliesWithoutAUsableObject(t *testing.T) {
	cases := []struct {
		name    string
		reply   string
		wantErr string
	}{
		{
			name:    "no JSON at all",
			reply:   "I could not review this file.",
			wantErr: "contained no JSON object",
		},
		{
			name:    "truncated JSON",
			reply:   `{"summary":"ok"`,
			wantErr: "contained no JSON object",
		},
		{
			// The braces balance but the array never closes, so there is no
			// JSON object in the reply to read a review out of.
			name:    "a broken object",
			reply:   `{"issues": [}`,
			wantErr: "contained no JSON object",
		},
		{
			name:    "the wrong shape",
			reply:   `{"issues": "many"}`,
			wantErr: "not the JSON that was asked for",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := ParseReview(testCase.reply)
			if err == nil {
				t.Fatalf("ParseReview(%q) = nil, want an error", testCase.reply)
			}
			if !strings.Contains(err.Error(), testCase.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, testCase.wantErr)
			}
		})
	}
}

func TestPromptStatesTheGuidelinesAndTheLinterFindings(t *testing.T) {
	system, user := Prompt(openAITestRequest())

	systemWants := []string{
		"Rules you must follow:",
		"are facts produced by real tools",
		"CRITICAL for a committed secret",
		"Use the line numbers of the new file",
		"Reply with JSON only. No code fences",
	}
	for _, want := range systemWants {
		if !strings.Contains(system, want) {
			t.Errorf("system prompt is missing %q:\n%s", want, system)
		}
	}

	userWants := []string{
		"Repository: acme/widget\n",
		"Pull request #7: Add retry logic\n",
		"Author: dev\n",
		"What the author says it does:\nRetries the flaky call.\n",
		"The team's review guidelines:\n1. Never commit secrets.\n2. Prefer early returns.\n",
		"File under review: internal/httpx/httpx.go\n",
		"Language: Go\n",
		"Static analysis findings for this file:\n- line 12 [S105] ruff (CRITICAL): possible hardcoded password\n",
		"Diff of this file:\n@@ -1,1 +1,2 @@\n-old\n+new\n\n",
	}
	for _, want := range userWants {
		if !strings.Contains(user, want) {
			t.Errorf("user prompt is missing %q:\n%s", want, user)
		}
	}
}

func TestPromptSaysThereAreNoFindingsAndOmitsEmptySections(t *testing.T) {
	request := openAITestRequest()
	request.Findings = nil
	request.Guidelines = nil
	request.Author = ""
	request.Description = "   "
	request.Language = ""

	_, user := Prompt(request)

	if !strings.Contains(user, "Static analysis findings for this file:\nnone\n") {
		t.Errorf("user prompt = %s, want it to say there are no findings", user)
	}
	for _, unwanted := range []string{"Static analysis findings for this file:\n- ", "The team's review guidelines:", "Author:", "What the author says it does:", "Language:"} {
		if strings.Contains(user, unwanted) {
			t.Errorf("user prompt contains %q, want the empty section left out:\n%s", unwanted, user)
		}
	}
}

func TestOpenAISendsTheDocumentedRequestAndParsesTheReply(t *testing.T) {
	content := `{"summary":"  Tighten the retry loop  ",
		"issues":[{"severity":"high","file":"internal/httpx/httpx.go","line":42,"title":"  Unbounded loop  ","detail":"  The loop never ends.  "}],
		"suggestions":["  Add a test  ","   "],
		"verdict":"  approve  "}`

	capture := &requestCapture{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capture.record(r)
		replyWith(t, map[string]any{
			"choices": []any{map[string]any{"message": map[string]string{"content": content}}},
			"usage":   map[string]int{"prompt_tokens": 11, "completion_tokens": 22},
		})(w, r)
	}))
	defer server.Close()

	policy, sleeps := testPolicy(3)
	provider := &OpenAI{
		BaseURL:   server.URL + "/",
		APIKey:    "test-key",
		Model:     "gpt-4o-mini",
		MaxTokens: 512,
		Timeout:   5 * time.Second,
		HTTP:      server.Client(),
		Policy:    policy,
	}

	review, usage, err := provider.Review(context.Background(), openAITestRequest())
	if err != nil {
		t.Fatalf("Review() = %v, want nil", err)
	}

	method, path, header, body := capture.snapshot()
	if method != http.MethodPost {
		t.Errorf("method = %s, want POST", method)
	}
	if path != "/chat/completions" {
		t.Errorf("path = %q, want /chat/completions with the trailing slash trimmed", path)
	}
	if got := header.Get("Authorization"); got != "Bearer test-key" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer test-key")
	}
	if got := header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if got := header.Get("User-Agent"); got != "code-review-agent" {
		t.Errorf("User-Agent = %q, want code-review-agent", got)
	}

	var payload struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		Temperature    float64 `json:"temperature"`
		MaxTokens      int     `json:"max_tokens"`
		ResponseFormat struct {
			Type string `json:"type"`
		} `json:"response_format"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("the request body %s is not the documented JSON: %v", body, err)
	}
	if payload.Model != "gpt-4o-mini" {
		t.Errorf("model = %q, want gpt-4o-mini", payload.Model)
	}
	if len(payload.Messages) != 2 {
		t.Fatalf("messages = %+v, want a system and a user message", payload.Messages)
	}
	if payload.Messages[0].Role != "system" || payload.Messages[1].Role != "user" {
		t.Errorf("message roles = %q, %q, want system, user", payload.Messages[0].Role, payload.Messages[1].Role)
	}
	if !strings.Contains(payload.Messages[0].Content, "Rules you must follow:") {
		t.Errorf("the system message = %q, want the review instructions", payload.Messages[0].Content)
	}
	if !strings.Contains(payload.Messages[1].Content, "Diff of this file:") {
		t.Errorf("the user message = %q, want the diff", payload.Messages[1].Content)
	}
	if payload.ResponseFormat.Type != "json_object" {
		t.Errorf("response_format.type = %q, want json_object", payload.ResponseFormat.Type)
	}
	if payload.Temperature != 0 {
		t.Errorf("temperature = %v, want 0", payload.Temperature)
	}
	if payload.MaxTokens != 512 {
		t.Errorf("max_tokens = %d, want 512", payload.MaxTokens)
	}
	if !strings.Contains(string(body), `"response_format":{"type":"json_object"}`) {
		t.Errorf("body = %s, want the exact response_format object", body)
	}

	if review.Summary != "Tighten the retry loop" {
		t.Errorf("Summary = %q, want it trimmed", review.Summary)
	}
	if len(review.Issues) != 1 {
		t.Fatalf("issues = %+v, want one issue", review.Issues)
	}
	issue := review.Issues[0]
	if issue.Severity != config.SeverityCritical {
		t.Errorf("Severity = %q, want %q", issue.Severity, config.SeverityCritical)
	}
	if issue.File != "internal/httpx/httpx.go" || issue.Line != 42 {
		t.Errorf("issue = %+v, want the file and line from the reply", issue)
	}
	if issue.Title != "Unbounded loop" || issue.Detail != "The loop never ends." {
		t.Errorf("issue = %+v, want the trimmed title and detail", issue)
	}
	if len(review.Suggestions) != 1 || review.Suggestions[0] != "Add a test" {
		t.Errorf("suggestions = %v, want only the one that says something", review.Suggestions)
	}
	if review.Verdict != "approve" {
		t.Errorf("Verdict = %q, want approve", review.Verdict)
	}

	if usage.Model != "gpt-4o-mini" {
		t.Errorf("usage.Model = %q, want the model name", usage.Model)
	}
	if usage.PromptTokens != 11 || usage.CompletionTokens != 22 {
		t.Errorf("usage = %+v, want the token counts from the reply", usage)
	}
	if usage.Duration < 0 {
		t.Errorf("usage.Duration = %v, want it not negative", usage.Duration)
	}
	if sleeps.count() != 0 {
		t.Errorf("waits = %d, want none on the first success", sleeps.count())
	}
}

func TestOpenAIRejectsAnEmptyBaseURL(t *testing.T) {
	policy, _ := testPolicy(3)
	provider := &OpenAI{APIKey: "k", Model: "m", HTTP: &http.Client{}, Policy: policy}
	_, _, err := provider.Review(context.Background(), openAITestRequest())
	if err == nil {
		t.Fatal("Review() = nil, want an error when no base URL is configured")
	}
	if !strings.Contains(err.Error(), "no base URL configured") {
		t.Errorf("error = %q, want it to name the missing base URL", err)
	}
}

func TestOpenAIRejectsARepliesItCannotUse(t *testing.T) {
	cases := []struct {
		name    string
		reply   string
		wantErr string
	}{
		{name: "no choices", reply: `{"choices":[],"usage":{}}`, wantErr: "the reply had no choices"},
		{name: "not JSON", reply: `not json at all`, wantErr: "could not decode the reply"},
		{name: "no JSON object in the content", reply: `{"choices":[{"message":{"content":"sorry"}}]}`, wantErr: "contained no JSON object"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, testCase.reply)
			}))
			defer server.Close()

			policy, _ := testPolicy(1)
			provider := &OpenAI{
				BaseURL: server.URL,
				APIKey:  "k",
				Model:   "m",
				HTTP:    server.Client(),
				Policy:  policy,
			}
			_, _, err := provider.Review(context.Background(), openAITestRequest())
			if err == nil {
				t.Fatalf("Review() = nil, want an error for %s", testCase.name)
			}
			if !strings.Contains(err.Error(), testCase.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, testCase.wantErr)
			}
		})
	}
}

func TestAnthropicSendsTheDocumentedRequestAndParsesTheMessagesReply(t *testing.T) {
	capture := &requestCapture{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capture.record(r)
		replyWith(t, map[string]any{
			"content": []any{
				map[string]string{"type": "text", "text": `{"summary":"two blocks",`},
				map[string]string{"type": "thinking", "text": `{"summary":"IGNORED"}`},
				map[string]string{"type": "text", "text": `"verdict":"approve"}`},
			},
			"usage": map[string]int{"input_tokens": 7, "output_tokens": 9},
		})(w, r)
	}))
	defer server.Close()

	policy, sleeps := testPolicy(3)
	provider := &Anthropic{
		BaseURL: server.URL,
		APIKey:  "anthropic-key",
		Model:   "claude-3-5-sonnet",
		HTTP:    server.Client(),
		Policy:  policy,
	}

	review, usage, err := provider.Review(context.Background(), openAITestRequest())
	if err != nil {
		t.Fatalf("Review() = %v, want nil", err)
	}

	method, path, header, body := capture.snapshot()
	if method != http.MethodPost {
		t.Errorf("method = %s, want POST", method)
	}
	if path != "/messages" {
		t.Errorf("path = %q, want /messages", path)
	}
	if got := header.Get("x-api-key"); got != "anthropic-key" {
		t.Errorf("x-api-key = %q, want anthropic-key", got)
	}
	if got := header.Get("anthropic-version"); got != "2023-06-01" {
		t.Errorf("anthropic-version = %q, want 2023-06-01", got)
	}
	if got := header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if got := header.Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q, want an Anthropic client to use x-api-key only", got)
	}

	var payload struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
		System    string `json:"system"`
		Messages  []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("the request body %s is not the documented JSON: %v", body, err)
	}
	if payload.Model != "claude-3-5-sonnet" {
		t.Errorf("model = %q, want claude-3-5-sonnet", payload.Model)
	}
	if payload.MaxTokens != 1500 {
		t.Errorf("max_tokens = %d, want the 1500 default when none is configured", payload.MaxTokens)
	}
	if !strings.Contains(payload.System, "Rules you must follow:") {
		t.Errorf("system = %q, want the review instructions", payload.System)
	}
	if len(payload.Messages) != 1 || payload.Messages[0].Role != "user" {
		t.Fatalf("messages = %+v, want one user message", payload.Messages)
	}
	if !strings.Contains(payload.Messages[0].Content, "Diff of this file:") {
		t.Errorf("the user message = %q, want the diff", payload.Messages[0].Content)
	}

	if review.Summary != "two blocks" {
		t.Errorf("Summary = %q, want the text blocks concatenated", review.Summary)
	}
	if review.Verdict != "approve" {
		t.Errorf("Verdict = %q, want approve", review.Verdict)
	}
	if len(review.Issues) != 0 {
		t.Errorf("issues = %+v, want none", review.Issues)
	}
	if usage.Model != "claude-3-5-sonnet" || usage.PromptTokens != 7 || usage.CompletionTokens != 9 {
		t.Errorf("usage = %+v, want the model and the input/output token counts", usage)
	}
	if sleeps.count() != 0 {
		t.Errorf("waits = %d, want none on the first success", sleeps.count())
	}
}

func TestAnthropicHonoursAnExplicitMaxTokens(t *testing.T) {
	var captured []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		replyWith(t, map[string]any{"content": []any{map[string]string{"type": "text", "text": `{"summary":"ok"}`}}})(w, r)
	}))
	defer server.Close()

	policy, _ := testPolicy(1)
	provider := &Anthropic{
		BaseURL:   server.URL,
		APIKey:    "k",
		Model:     "m",
		MaxTokens: 256,
		HTTP:      server.Client(),
		Policy:    policy,
	}
	if _, _, err := provider.Review(context.Background(), openAITestRequest()); err != nil {
		t.Fatalf("Review() = %v, want nil", err)
	}
	if !strings.Contains(string(captured), `"max_tokens":256`) {
		t.Errorf("body = %s, want max_tokens to be 256", captured)
	}
}

func TestProvidersRetryServerFailuresAndGiveUpOnClientErrors(t *testing.T) {
	providers := []struct {
		name string
		// newProvider points the provider at a server.
		newProvider func(server *httptest.Server, policy httpx.Policy) Provider
	}{
		{
			name: "openai",
			newProvider: func(server *httptest.Server, policy httpx.Policy) Provider {
				return &OpenAI{BaseURL: server.URL, APIKey: "k", Model: "m", HTTP: server.Client(), Policy: policy}
			},
		},
		{
			name: "anthropic",
			newProvider: func(server *httptest.Server, policy httpx.Policy) Provider {
				return &Anthropic{BaseURL: server.URL, APIKey: "k", Model: "m", HTTP: server.Client(), Policy: policy}
			},
		},
	}

	for _, entry := range providers {
		t.Run(entry.name, func(t *testing.T) {
			t.Run("a 500 is retried and then reported", func(t *testing.T) {
				var mutex sync.Mutex
				var calls int
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mutex.Lock()
					calls++
					mutex.Unlock()
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = io.WriteString(w, `{"error":"the model is down"}`)
				}))
				defer server.Close()

				policy, sleeps := testPolicy(3)
				provider := entry.newProvider(server, policy)
				_, _, err := provider.Review(context.Background(), openAITestRequest())
				if err == nil {
					t.Fatal("Review() = nil, want an error after the retries")
				}
				if !strings.Contains(err.Error(), "answered 500") {
					t.Errorf("error = %q, want it to name the status", err)
				}
				if !strings.Contains(err.Error(), "the model is down") {
					t.Errorf("error = %q, want the body included", err)
				}
				mutex.Lock()
				got := calls
				mutex.Unlock()
				if got != 3 {
					t.Errorf("requests = %d, want 3", got)
				}
				if sleeps.count() != 2 {
					t.Errorf("waits = %d, want 2", sleeps.count())
				}
			})

			t.Run("a 400 is not retried", func(t *testing.T) {
				var mutex sync.Mutex
				var calls int
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mutex.Lock()
					calls++
					mutex.Unlock()
					w.WriteHeader(http.StatusBadRequest)
					_, _ = io.WriteString(w, `{"error":"bad api key"}`)
				}))
				defer server.Close()

				policy, sleeps := testPolicy(3)
				provider := entry.newProvider(server, policy)
				_, _, err := provider.Review(context.Background(), openAITestRequest())
				if err == nil {
					t.Fatal("Review() = nil, want an error")
				}
				if !strings.Contains(err.Error(), "answered 400") || !strings.Contains(err.Error(), "bad api key") {
					t.Errorf("error = %q, want the status and the body", err)
				}
				mutex.Lock()
				got := calls
				mutex.Unlock()
				if got != 1 {
					t.Errorf("requests = %d, want 1: a 400 must not be retried", got)
				}
				if sleeps.count() != 0 {
					t.Errorf("waits = %d, want none", sleeps.count())
				}
			})

			t.Run("a 503 followed by a success is retried once", func(t *testing.T) {
				var mutex sync.Mutex
				var calls int
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mutex.Lock()
					calls++
					attempt := calls
					mutex.Unlock()
					if attempt == 1 {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					if entry.name == "openai" {
						replyWith(t, map[string]any{
							"choices": []any{map[string]any{"message": map[string]string{"content": `{"summary":"ok"}`}}},
						})(w, r)
						return
					}
					replyWith(t, map[string]any{
						"content": []any{map[string]string{"type": "text", "text": `{"summary":"ok"}`}},
					})(w, r)
				}))
				defer server.Close()

				policy, sleeps := testPolicy(3)
				provider := entry.newProvider(server, policy)
				review, _, err := provider.Review(context.Background(), openAITestRequest())
				if err != nil {
					t.Fatalf("Review() = %v, want nil after the retry", err)
				}
				if review.Summary != "ok" {
					t.Errorf("Summary = %q, want the parsed reply", review.Summary)
				}
				mutex.Lock()
				got := calls
				mutex.Unlock()
				if got != 2 {
					t.Errorf("requests = %d, want 2", got)
				}
				if sleeps.count() != 1 {
					t.Errorf("waits = %d, want 1", sleeps.count())
				}
			})
		})
	}
}

func TestNoneReturnsAnEmptyReviewAndNoError(t *testing.T) {
	var provider Provider = None{}
	if got := provider.Name(); got != "none" {
		t.Errorf("Name() = %q, want none", got)
	}
	review, usage, err := provider.Review(context.Background(), openAITestRequest())
	if err != nil {
		t.Fatalf("Review() = %v, want nil", err)
	}
	if review.Verdict != "no model configured" {
		t.Errorf("Verdict = %q, want the documented placeholder", review.Verdict)
	}
	if len(review.Issues) != 0 || len(review.Suggestions) != 0 {
		t.Errorf("review = %+v, want nothing but the verdict", review)
	}
	if usage.Model != "none" {
		t.Errorf("usage.Model = %q, want none", usage.Model)
	}
}

func TestNewSelectsTheProviderTheConfigurationNames(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		check    func(t *testing.T, provider Provider)
	}{
		{
			name:     "an empty provider is none",
			provider: "",
			check: func(t *testing.T, provider Provider) {
				if _, ok := provider.(None); !ok {
					t.Errorf("provider = %T, want None", provider)
				}
			},
		},
		{
			name:     "none is none",
			provider: "none",
			check: func(t *testing.T, provider Provider) {
				if _, ok := provider.(None); !ok {
					t.Errorf("provider = %T, want None", provider)
				}
			},
		},
		{
			name:     "openai",
			provider: "openai",
			check: func(t *testing.T, provider Provider) {
				openai, ok := provider.(*OpenAI)
				if !ok {
					t.Fatalf("provider = %T, want *OpenAI", provider)
				}
				if openai.Model != "gpt-4o-mini" || openai.BaseURL != "http://example.test/v1" {
					t.Errorf("provider = %+v, want the configured model and base URL", openai)
				}
				if openai.APIKey != "secret" {
					t.Errorf("APIKey = %q, want the value the lookup returned", openai.APIKey)
				}
				if openai.MaxTokens != 321 {
					t.Errorf("MaxTokens = %d, want 321", openai.MaxTokens)
				}
				if openai.Timeout != 45*time.Second {
					t.Errorf("Timeout = %v, want 45s", openai.Timeout)
				}
				if openai.HTTP == nil {
					t.Error("HTTP = nil, want a client")
				}
				if openai.Policy.Attempts != 3 {
					t.Errorf("Policy.Attempts = %d, want the default policy", openai.Policy.Attempts)
				}
			},
		},
		{
			name:     "ollama speaks the openai shape",
			provider: "ollama",
			check: func(t *testing.T, provider Provider) {
				if _, ok := provider.(*OpenAI); !ok {
					t.Errorf("provider = %T, want *OpenAI", provider)
				}
			},
		},
		{
			name:     "anthropic",
			provider: "anthropic",
			check: func(t *testing.T, provider Provider) {
				anthropic, ok := provider.(*Anthropic)
				if !ok {
					t.Fatalf("provider = %T, want *Anthropic", provider)
				}
				if anthropic.Model != "gpt-4o-mini" || anthropic.APIKey != "secret" {
					t.Errorf("provider = %+v, want the configured model and key", anthropic)
				}
				if anthropic.HTTP == nil || anthropic.Policy.Attempts != 3 {
					t.Errorf("provider = %+v, want a client and the default policy", anthropic)
				}
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Model.Provider = testCase.provider
			cfg.Model.Name = "gpt-4o-mini"
			cfg.Model.BaseURL = "http://example.test/v1"
			cfg.Model.APIKeyEnv = "MY_KEY"
			cfg.Model.MaxTokens = 321
			cfg.Model.Timeout = config.Seconds(45 * time.Second)

			var asked []string
			provider, err := New(cfg, func(name string) string {
				asked = append(asked, name)
				return "secret"
			})
			if err != nil {
				t.Fatalf("New() = %v, want nil", err)
			}
			testCase.check(t, provider)

			if testCase.provider != "" && testCase.provider != "none" {
				if len(asked) != 1 || asked[0] != "MY_KEY" {
					t.Errorf("looked up %v, want only MY_KEY", asked)
				}
			} else if len(asked) != 0 {
				t.Errorf("looked up %v, want no environment lookup for %q", asked, testCase.provider)
			}
		})
	}
}

func TestNewRejectsAnUnknownProvider(t *testing.T) {
	cfg := config.Default()
	cfg.Model.Provider = "gemini"
	cfg.Model.Name = "gemini-pro"

	provider, err := New(cfg, func(string) string { return "secret" })
	if err == nil {
		t.Fatal("New() = nil, want an error for an unknown provider")
	}
	if provider != nil {
		t.Errorf("provider = %T, want nil", provider)
	}
	if !strings.Contains(err.Error(), `unknown provider "gemini"`) {
		t.Errorf("error = %q, want it to name the provider", err)
	}
}

func TestNewWithNoProviderNeedsNoEnvironmentLookup(t *testing.T) {
	cfg := config.Default()
	cfg.Model.Provider = "none"
	provider, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New() = %v, want nil", err)
	}
	if _, ok := provider.(None); !ok {
		t.Errorf("provider = %T, want None", provider)
	}
}

func TestProviderNamesIncludeTheModel(t *testing.T) {
	openai := &OpenAI{Model: "gpt-4o-mini"}
	if got := openai.Name(); got != "openai-compatible/gpt-4o-mini" {
		t.Errorf("OpenAI.Name() = %q, want openai-compatible/gpt-4o-mini", got)
	}
	anthropic := &Anthropic{Model: "claude-3-5-sonnet"}
	if got := anthropic.Name(); got != "anthropic/claude-3-5-sonnet" {
		t.Errorf("Anthropic.Name() = %q, want anthropic/claude-3-5-sonnet", got)
	}
}
