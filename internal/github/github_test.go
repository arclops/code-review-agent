package github

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/arclops/code-review-agent/internal/httpx"
)

// testClient points a client at a test server and makes its retries unable to
// sleep, so that every request the code makes is one the test can count.
func testClient(t *testing.T, server *httptest.Server, token string) *Client {
	t.Helper()
	client := New(server.URL+"/", token)
	client.HTTP = server.Client()
	client.Policy = httpx.Policy{
		Attempts: 1,
		Sleep:    func(context.Context, time.Duration) error { return nil },
	}
	return client
}

func TestPullRequestSendsTheDocumentedHeadersAndDecodesTheBody(t *testing.T) {
	const body = `{
		"number": 42,
		"title": "Add a linter",
		"body": "It lints.",
		"state": "open",
		"draft": true,
		"user": {"login": "octocat", "type": "User"},
		"head": {"ref": "feature/lint", "sha": "abc123"},
		"base": {"ref": "main", "sha": "def456"},
		"html_url": "https://github.com/octo/hello/pull/42",
		"additions": 12,
		"deletions": 3,
		"changed_files": 2,
		"created_at": "2024-05-01T12:30:00Z",
		"updated_at": "2024-05-02T08:00:00Z"
	}`

	var method, path, query string
	var headers http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, query, headers = r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	defer server.Close()

	pull, err := testClient(t, server, "test-token").PullRequest(context.Background(), "octo/hello", 42)
	if err != nil {
		t.Fatalf("PullRequest returned an unexpected error: %v", err)
	}

	if method != http.MethodGet {
		t.Errorf("method = %q, want %q", method, http.MethodGet)
	}
	if path != "/repos/octo/hello/pulls/42" {
		t.Errorf("path = %q, want %q", path, "/repos/octo/hello/pulls/42")
	}
	if query != "" {
		t.Errorf("query = %q, want no query", query)
	}
	for _, want := range []struct{ header, value string }{
		{"Accept", "application/vnd.github+json"},
		{"X-GitHub-Api-Version", "2022-11-28"},
		{"User-Agent", "code-review-agent"},
		{"Authorization", "Bearer test-token"},
	} {
		if got := headers.Get(want.header); got != want.value {
			t.Errorf("%s header = %q, want %q", want.header, got, want.value)
		}
	}

	want := PullRequest{
		Number:       42,
		Title:        "Add a linter",
		Body:         "It lints.",
		State:        "open",
		Draft:        true,
		User:         User{Login: "octocat", Type: "User"},
		Head:         Branch{Ref: "feature/lint", SHA: "abc123"},
		Base:         Branch{Ref: "main", SHA: "def456"},
		HTMLURL:      "https://github.com/octo/hello/pull/42",
		Additions:    12,
		Deletions:    3,
		ChangedFiles: 2,
		CreatedAt:    time.Date(2024, 5, 1, 12, 30, 0, 0, time.UTC),
		UpdatedAt:    time.Date(2024, 5, 2, 8, 0, 0, 0, time.UTC),
	}
	if !reflect.DeepEqual(pull, want) {
		t.Errorf("PullRequest = %+v, want %+v", pull, want)
	}
}

func TestClientWithoutATokenSendsNoAuthorizationHeader(t *testing.T) {
	var headers http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers = r.Header.Clone()
		_, _ = io.WriteString(w, `{"number":1}`)
	}))
	defer server.Close()

	if _, err := testClient(t, server, "").PullRequest(context.Background(), "octo/hello", 1); err != nil {
		t.Fatalf("PullRequest returned an unexpected error: %v", err)
	}
	if _, ok := headers["Authorization"]; ok {
		t.Errorf("Authorization header = %q, want it absent", headers.Get("Authorization"))
	}
	if got := headers.Get("User-Agent"); got != "code-review-agent" {
		t.Errorf("User-Agent header = %q, want %q", got, "code-review-agent")
	}
}

func TestFilesFollowsTheLinkHeaderAndReturnsEveryPageInOrder(t *testing.T) {
	var requested []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = append(requested, r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "2" {
			_, _ = io.WriteString(w, `[{"filename":"c.go","status":"added","additions":5,"deletions":0,"changes":5,"patch":"@@ -0,0 +1,5 @@","blob_url":"https://github.com/octo/hello/blob/abc123/c.go"}]`)
			return
		}
		// The same Link header the real API sends: absolute, and on its own
		// host, which is what the client has to rewrite to the configured base.
		w.Header().Set("Link", `<https://api.github.com/repos/octo/hello/pulls/7/files?per_page=100&page=2>; rel="next", <https://api.github.com/repos/octo/hello/pulls/7/files?per_page=100&page=2>; rel="last"`)
		_, _ = io.WriteString(w, `[
			{"filename":"a.go","status":"modified","additions":1,"deletions":1,"changes":2,"patch":"@@ -1 +1 @@","blob_url":"https://github.com/octo/hello/blob/abc123/a.go"},
			{"filename":"b.go","status":"modified","additions":2,"deletions":0,"changes":2,"patch":"","blob_url":"https://github.com/octo/hello/blob/abc123/b.go"}
		]`)
	}))
	defer server.Close()

	files, err := testClient(t, server, "test-token").Files(context.Background(), "octo/hello", 7)
	if err != nil {
		t.Fatalf("Files returned an unexpected error: %v", err)
	}

	wantRequests := []string{
		"/repos/octo/hello/pulls/7/files?per_page=100",
		"/repos/octo/hello/pulls/7/files?per_page=100&page=2",
	}
	if !reflect.DeepEqual(requested, wantRequests) {
		t.Fatalf("requested %q, want %q (page two must be served by this test server)", requested, wantRequests)
	}

	var names []string
	for _, file := range files {
		names = append(names, file.Filename)
	}
	if want := []string{"a.go", "b.go", "c.go"}; !reflect.DeepEqual(names, want) {
		t.Errorf("filenames = %q, want %q", names, want)
	}
	want := File{
		Filename:  "a.go",
		Status:    "modified",
		Additions: 1,
		Deletions: 1,
		Changes:   2,
		Patch:     "@@ -1 +1 @@",
		BlobURL:   "https://github.com/octo/hello/blob/abc123/a.go",
	}
	if !reflect.DeepEqual(files[0], want) {
		t.Errorf("first file = %+v, want %+v", files[0], want)
	}
}

func TestCommentsFollowsPaginationWithTheQueryTheFirstPageAsked(t *testing.T) {
	var requested []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = append(requested, r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("page") {
		case "":
			w.Header().Set("Link", `<https://api.github.com/repos/octo/hello/issues/7/comments?per_page=100&page=2>; rel="next"`)
			_, _ = io.WriteString(w, `[{"id":11,"body":"first","user":{"login":"octocat","type":"User"},"html_url":"https://github.com/octo/hello/pull/7#issuecomment-11","created_at":"2024-05-01T10:00:00Z","updated_at":"2024-05-01T10:00:00Z"}]`)
		case "2":
			_, _ = io.WriteString(w, `[{"id":12,"body":"second","user":{"login":"reviewer","type":"User"},"html_url":"https://github.com/octo/hello/pull/7#issuecomment-12","created_at":"2024-05-01T11:00:00Z","updated_at":"2024-05-01T11:30:00Z"}]`)
		default:
			http.Error(w, `{"message":"unexpected page"}`, http.StatusNotFound)
		}
	}))
	defer server.Close()

	comments, err := testClient(t, server, "test-token").Comments(context.Background(), "octo/hello", 7)
	if err != nil {
		t.Fatalf("Comments returned an unexpected error: %v", err)
	}
	wantRequests := []string{
		"/repos/octo/hello/issues/7/comments?per_page=100",
		"/repos/octo/hello/issues/7/comments?per_page=100&page=2",
	}
	if !reflect.DeepEqual(requested, wantRequests) {
		t.Fatalf("requested %q, want %q", requested, wantRequests)
	}
	if len(comments) != 2 {
		t.Fatalf("got %d comments, want 2", len(comments))
	}
	if comments[0].ID != 11 || comments[1].ID != 12 {
		t.Errorf("comment ids = %d, %d, want 11, 12 in order", comments[0].ID, comments[1].ID)
	}
	if comments[1].Body != "second" || comments[1].User.Login != "reviewer" {
		t.Errorf("second comment = %+v, want body %q from %q", comments[1], "second", "reviewer")
	}
	if want := time.Date(2024, 5, 1, 11, 30, 0, 0, time.UTC); !comments[1].UpdatedAt.Equal(want) {
		t.Errorf("second comment UpdatedAt = %s, want %s", comments[1].UpdatedAt, want)
	}
}

func TestCreateCommentPostsTheBodyAsJSONAndDecodesTheCreatedComment(t *testing.T) {
	var method, path, contentType string
	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, contentType = r.Method, r.URL.Path, r.Header.Get("Content-Type")
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":99,"body":"a review comment","user":{"login":"bot","type":"Bot"},"html_url":"https://github.com/octo/hello/pull/7#issuecomment-99","created_at":"2024-05-03T09:00:00Z"}`)
	}))
	defer server.Close()

	comment, err := testClient(t, server, "test-token").CreateComment(context.Background(), "octo/hello", 7, "a review comment")
	if err != nil {
		t.Fatalf("CreateComment returned an unexpected error: %v", err)
	}

	if method != http.MethodPost {
		t.Errorf("method = %q, want %q", method, http.MethodPost)
	}
	if path != "/repos/octo/hello/issues/7/comments" {
		t.Errorf("path = %q, want %q", path, "/repos/octo/hello/issues/7/comments")
	}
	if contentType != "application/json" {
		t.Errorf("Content-Type = %q, want %q", contentType, "application/json")
	}
	if got := string(body); got != `{"body":"a review comment"}` {
		t.Errorf("request body = %s, want %s", got, `{"body":"a review comment"}`)
	}
	want := Comment{
		ID:        99,
		Body:      "a review comment",
		User:      User{Login: "bot", Type: "Bot"},
		HTMLURL:   "https://github.com/octo/hello/pull/7#issuecomment-99",
		CreatedAt: time.Date(2024, 5, 3, 9, 0, 0, 0, time.UTC),
	}
	if !reflect.DeepEqual(comment, want) {
		t.Errorf("created comment = %+v, want %+v", comment, want)
	}
}

func TestUpdateCommentPatchesTheRightPathWithTheNewBody(t *testing.T) {
	var method, path string
	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":99,"body":"a rewritten review comment","user":{"login":"bot","type":"Bot"},"html_url":"https://github.com/octo/hello/pull/7#issuecomment-99","created_at":"2024-05-03T09:00:00Z","updated_at":"2024-05-03T10:15:00Z"}`)
	}))
	defer server.Close()

	comment, err := testClient(t, server, "test-token").UpdateComment(context.Background(), "octo/hello", 99, "a rewritten review comment")
	if err != nil {
		t.Fatalf("UpdateComment returned an unexpected error: %v", err)
	}

	if method != http.MethodPatch {
		t.Errorf("method = %q, want %q", method, http.MethodPatch)
	}
	if path != "/repos/octo/hello/issues/comments/99" {
		t.Errorf("path = %q, want %q", path, "/repos/octo/hello/issues/comments/99")
	}
	if got := string(body); got != `{"body":"a rewritten review comment"}` {
		t.Errorf("request body = %s, want %s", got, `{"body":"a rewritten review comment"}`)
	}
	if comment.ID != 99 || comment.Body != "a rewritten review comment" {
		t.Errorf("updated comment = %+v, want id 99 with the new body", comment)
	}
	if want := time.Date(2024, 5, 3, 10, 15, 0, 0, time.UTC); !comment.UpdatedAt.Equal(want) {
		t.Errorf("UpdatedAt = %s, want %s", comment.UpdatedAt, want)
	}
}

func TestARefusalBecomesAnAPIErrorCarryingTheStatusAndTheAPIMessage(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		body        string
		method      string
		path        string
		wantMessage string
		call        func(*Client) error
	}{
		{
			name:        "404 with the API's JSON body",
			status:      http.StatusNotFound,
			body:        `{"message":"Not Found","documentation_url":"https://docs.github.com/rest/pulls/pulls#get-a-pull-request"}`,
			method:      http.MethodGet,
			path:        "/repos/octo/hello/pulls/7",
			wantMessage: `{"message":"Not Found","documentation_url":"https://docs.github.com/rest/pulls/pulls#get-a-pull-request"}`,
			call:        func(c *Client) error { _, err := c.PullRequest(context.Background(), "octo/hello", 7); return err },
		},
		{
			name:        "422 from the comments endpoint",
			status:      http.StatusUnprocessableEntity,
			body:        `{"message":"Validation Failed","errors":[{"resource":"IssueComment","code":"unprocessable"}]}`,
			method:      http.MethodPost,
			path:        "/repos/octo/hello/issues/7/comments",
			wantMessage: `{"message":"Validation Failed","errors":[{"resource":"IssueComment","code":"unprocessable"}]}`,
			call: func(c *Client) error {
				_, err := c.CreateComment(context.Background(), "octo/hello", 7, "hi")
				return err
			},
		},
		{
			name:        "404 with an HTML body",
			status:      http.StatusNotFound,
			body:        "<html><body>no such pull request</body></html>",
			method:      http.MethodGet,
			path:        "/repos/octo/hello/pulls/7",
			wantMessage: "<html><body>no such pull request</body></html>",
			call:        func(c *Client) error { _, err := c.PullRequest(context.Background(), "octo/hello", 7); return err },
		},
		{
			name:        "500 with a plain text body",
			status:      http.StatusInternalServerError,
			body:        "upstream boom\n",
			method:      http.MethodGet,
			path:        "/repos/octo/hello/pulls/7",
			wantMessage: "upstream boom",
			call:        func(c *Client) error { _, err := c.PullRequest(context.Background(), "octo/hello", 7); return err },
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()

			err := test.call(testClient(t, server, "test-token"))
			if err == nil {
				t.Fatalf("want an error for status %d, got nil", test.status)
			}
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("error type = %T (%v), want *APIError", err, err)
			}
			if apiErr.Status != test.status {
				t.Errorf("Status = %d, want %d", apiErr.Status, test.status)
			}
			if apiErr.Method != test.method {
				t.Errorf("Method = %q, want %q", apiErr.Method, test.method)
			}
			if apiErr.Path != test.path {
				t.Errorf("Path = %q, want %q", apiErr.Path, test.path)
			}
			if apiErr.Message != test.wantMessage {
				t.Errorf("Message = %q, want %q", apiErr.Message, test.wantMessage)
			}
			want := fmt.Sprintf("github: %s %s returned %d: %s", test.method, test.path, test.status, test.wantMessage)
			if got := apiErr.Error(); got != want {
				t.Errorf("Error() = %q, want %q", got, want)
			}
		})
	}
}

func TestFilesRefusalNamesThePaginatedPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
	}))
	defer server.Close()

	_, err := testClient(t, server, "test-token").Files(context.Background(), "octo/hello", 7)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %v, want *APIError", err)
	}
	if want := "/repos/octo/hello/pulls/7/files?per_page=100"; apiErr.Path != want {
		t.Errorf("Path = %q, want %q", apiErr.Path, want)
	}
	if apiErr.Status != http.StatusNotFound {
		t.Errorf("Status = %d, want %d", apiErr.Status, http.StatusNotFound)
	}
}

func TestRetryableStatusesAreRetriedAndOtherRefusalsAreNot(t *testing.T) {
	tests := []struct {
		name        string
		statuses    []int
		wantCalls   int
		wantSlept   []time.Duration
		wantFailure bool
	}{
		{
			name:      "two server errors then a good answer",
			statuses:  []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusOK},
			wantCalls: 3,
			wantSlept: []time.Duration{5 * time.Millisecond, 10 * time.Millisecond},
		},
		{
			name:        "a client error is not retried",
			statuses:    []int{http.StatusNotFound},
			wantCalls:   1,
			wantFailure: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				status := test.statuses[len(test.statuses)-1]
				if calls < len(test.statuses) {
					status = test.statuses[calls]
				}
				calls++
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"number":7}`)
			}))
			defer server.Close()

			var slept []time.Duration
			client := New(server.URL, "test-token")
			client.HTTP = server.Client()
			client.Policy = httpx.Policy{
				Attempts:  3,
				BaseDelay: 5 * time.Millisecond,
				Sleep: func(_ context.Context, d time.Duration) error {
					slept = append(slept, d)
					return nil
				},
			}

			_, err := client.PullRequest(context.Background(), "octo/hello", 7)
			if test.wantFailure && err == nil {
				t.Error("want an error, got nil")
			}
			if !test.wantFailure && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if calls != test.wantCalls {
				t.Errorf("made %d requests, want %d", calls, test.wantCalls)
			}
			if !reflect.DeepEqual(slept, test.wantSlept) {
				t.Errorf("slept %v, want %v", slept, test.wantSlept)
			}
		})
	}
}

func TestNextPagePutsAForeignLinkBackOnTheConfiguredBaseURL(t *testing.T) {
	tests := []struct {
		name   string
		header string
		base   string
		want   string
	}{
		{
			name:   "a link on api.github.com is moved onto the base",
			header: `<https://api.github.com/repos/o/n/pulls/1/files?per_page=100&page=2>; rel="next"`,
			base:   "http://127.0.0.1:8080",
			want:   "http://127.0.0.1:8080/repos/o/n/pulls/1/files?per_page=100&page=2",
		},
		{
			name:   "a scheme relative link is moved onto the base",
			header: `<//api.github.com/repos/o/n/pulls/1/files?page=2>; rel="next"`,
			base:   "http://127.0.0.1:8080",
			want:   "http://127.0.0.1:8080/repos/o/n/pulls/1/files?page=2",
		},
		{
			name:   "a link already on the base host is left alone",
			header: `<http://127.0.0.1:8080/repos/o/n/pulls/1/files?page=2>; rel="next"`,
			base:   "http://127.0.0.1:8080",
			want:   "http://127.0.0.1:8080/repos/o/n/pulls/1/files?page=2",
		},
		{
			name:   "next is found after last",
			header: `<https://api.github.com/repos/o/n/pulls/1/files?page=9>; rel="last", <https://api.github.com/repos/o/n/pulls/1/files?page=2>; rel="next"`,
			base:   "http://127.0.0.1:8080",
			want:   "http://127.0.0.1:8080/repos/o/n/pulls/1/files?page=2",
		},
		{
			name:   "a relative link is kept as it is",
			header: `</repos/o/n/pulls/1/files?page=2>; rel="next"`,
			base:   "http://127.0.0.1:8080",
			want:   "/repos/o/n/pulls/1/files?page=2",
		},
		{
			name:   "only last means there is no next page",
			header: `<https://api.github.com/repos/o/n/pulls/1/files?page=2>; rel="last"`,
			base:   "http://127.0.0.1:8080",
			want:   "",
		},
		{
			name:   "no Link header means there is no next page",
			header: "",
			base:   "http://127.0.0.1:8080",
			want:   "",
		},
		{
			name:   "text that is not a link is ignored",
			header: "next page please",
			base:   "http://127.0.0.1:8080",
			want:   "",
		},
		{
			name:   "an unusable base URL stops pagination",
			header: `<https://api.github.com/repos/o/n/pulls/1/files?page=2>; rel="next"`,
			base:   "://not a url",
			want:   "",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := nextPage(test.header, test.base); got != test.want {
				t.Errorf("nextPage(%q, %q) = %q, want %q", test.header, test.base, got, test.want)
			}
		})
	}
}

func TestNewFillsInDefaultsAndTrimsOneTrailingSlash(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		want    string
	}{
		{name: "no base URL means the public API", baseURL: "", want: DefaultBaseURL},
		{name: "a trailing slash is trimmed", baseURL: "https://github.example.com/", want: "https://github.example.com"},
		{name: "a base URL without one is kept", baseURL: "https://github.example.com", want: "https://github.example.com"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := New(test.baseURL, "test-token")
			if client.BaseURL != test.want {
				t.Errorf("BaseURL = %q, want %q", client.BaseURL, test.want)
			}
			if client.Token != "test-token" {
				t.Errorf("Token = %q, want %q", client.Token, "test-token")
			}
			if client.UserAgent != "code-review-agent" {
				t.Errorf("UserAgent = %q, want %q", client.UserAgent, "code-review-agent")
			}
			if client.HTTP == nil {
				t.Error("HTTP client is nil, want a usable default")
			}
			if client.Policy.Attempts != 3 {
				t.Errorf("Policy.Attempts = %d, want 3", client.Policy.Attempts)
			}
		})
	}
}

func TestABaseURLWithoutASchemeFailsInsteadOfBeingGuessedAt(t *testing.T) {
	client := New("api.github.com", "")
	client.Policy = httpx.Policy{Attempts: 1, Sleep: func(context.Context, time.Duration) error { return nil }}

	_, err := client.PullRequest(context.Background(), "octo/hello", 7)
	if err == nil {
		t.Fatal("want an error for a base URL without a scheme, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported protocol scheme") {
		t.Errorf("error = %v, want it to mention %q", err, "unsupported protocol scheme")
	}
}

func TestParseRepo(t *testing.T) {
	tests := []struct {
		repo      string
		wantOwner string
		wantName  string
		wantErr   bool
	}{
		{repo: "octo/hello", wantOwner: "octo", wantName: "hello"},
		{repo: "  octo/hello  ", wantOwner: "octo", wantName: "hello"},
		{repo: "octo/hello/extra", wantErr: true},
		{repo: "octo/hello/", wantErr: true},
		{repo: "octo", wantErr: true},
		{repo: "octo/", wantErr: true},
		{repo: "/hello", wantErr: true},
		{repo: "", wantErr: true},
		{repo: "   ", wantErr: true},
	}

	for _, test := range tests {
		t.Run(fmt.Sprintf("%q", test.repo), func(t *testing.T) {
			owner, name, err := ParseRepo(test.repo)
			if test.wantErr {
				if err == nil {
					t.Fatalf("ParseRepo(%q) = %q, %q, nil, want an error", test.repo, owner, name)
				}
				if !strings.Contains(err.Error(), "is not an owner/name repository") {
					t.Errorf("error = %v, want it to say the repository is not owner/name", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseRepo(%q) returned an unexpected error: %v", test.repo, err)
			}
			if owner != test.wantOwner || name != test.wantName {
				t.Errorf("ParseRepo(%q) = %q, %q, want %q, %q", test.repo, owner, name, test.wantOwner, test.wantName)
			}
		})
	}
}

func TestParseNumber(t *testing.T) {
	tests := []struct {
		value   string
		want    int
		wantErr bool
	}{
		{value: "42", want: 42},
		{value: " 7 ", want: 7},
		{value: "0", wantErr: true},
		{value: "-3", wantErr: true},
		{value: "12x", wantErr: true},
		{value: "abc", wantErr: true},
		{value: "", wantErr: true},
	}

	for _, test := range tests {
		t.Run(fmt.Sprintf("%q", test.value), func(t *testing.T) {
			number, err := ParseNumber(test.value)
			if test.wantErr {
				if err == nil {
					t.Fatalf("ParseNumber(%q) = %d, nil, want an error", test.value, number)
				}
				if !strings.Contains(err.Error(), "is not a pull request number") {
					t.Errorf("error = %v, want it to say the value is not a pull request number", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseNumber(%q) returned an unexpected error: %v", test.value, err)
			}
			if number != test.want {
				t.Errorf("ParseNumber(%q) = %d, want %d", test.value, number, test.want)
			}
		})
	}
}
