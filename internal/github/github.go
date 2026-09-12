// Package github is a small client for the parts of the GitHub REST API this
// service needs: a pull request, the files it changes, and the comments on it.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/arclops/code-review-agent/internal/httpx"
)

// DefaultBaseURL is the public API.
const DefaultBaseURL = "https://api.github.com"

// Client talks to GitHub.
type Client struct {
	BaseURL   string
	Token     string
	UserAgent string
	HTTP      *http.Client
	Policy    httpx.Policy
}

// New builds a client with sensible defaults filled in.
func New(baseURL, token string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		BaseURL:   strings.TrimSuffix(baseURL, "/"),
		Token:     token,
		UserAgent: "code-review-agent",
		HTTP:      &http.Client{Timeout: 30 * time.Second},
		Policy:    httpx.DefaultPolicy(),
	}
}

// User is the account behind a pull request or a comment.
type User struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

// Branch is one side of a pull request.
type Branch struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

// PullRequest is the part of a pull request this service reads.
type PullRequest struct {
	Number       int       `json:"number"`
	Title        string    `json:"title"`
	Body         string    `json:"body"`
	State        string    `json:"state"`
	Draft        bool      `json:"draft"`
	User         User      `json:"user"`
	Head         Branch    `json:"head"`
	Base         Branch    `json:"base"`
	HTMLURL      string    `json:"html_url"`
	Additions    int       `json:"additions"`
	Deletions    int       `json:"deletions"`
	ChangedFiles int       `json:"changed_files"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// File is one changed file. Patch is the unified diff and is absent for a
// binary file or one whose diff GitHub declines to produce.
type File struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Changes   int    `json:"changes"`
	Patch     string `json:"patch"`
	BlobURL   string `json:"blob_url"`
}

// Comment is a comment on the pull request's conversation.
type Comment struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	User      User      `json:"user"`
	HTMLURL   string    `json:"html_url"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// APIError is a response GitHub refused. The status is what the caller needs to
// decide whether to care, and the message is what a log needs.
type APIError struct {
	Status  int
	Method  string
	Path    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("github: %s %s returned %d: %s", e.Method, e.Path, e.Status, e.Message)
}

// PullRequest fetches one pull request.
func (c *Client) PullRequest(ctx context.Context, repo string, number int) (PullRequest, error) {
	var pull PullRequest
	err := c.get(ctx, fmt.Sprintf("/repos/%s/pulls/%d", repo, number), &pull)
	return pull, err
}

// Files fetches every changed file, following pagination: a large pull request
// does not fit in one page, and silently reviewing the first thirty files of a
// sixty file change would be worse than failing.
func (c *Client) Files(ctx context.Context, repo string, number int) ([]File, error) {
	return getAll[File](ctx, c, fmt.Sprintf("/repos/%s/pulls/%d/files?per_page=100", repo, number))
}

// Comments fetches every comment on the pull request's conversation.
func (c *Client) Comments(ctx context.Context, repo string, number int) ([]Comment, error) {
	return getAll[Comment](ctx, c, fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=100", repo, number))
}

// CreateComment posts a comment and returns it.
func (c *Client) CreateComment(ctx context.Context, repo string, number int, body string) (Comment, error) {
	path := fmt.Sprintf("/repos/%s/issues/%d/comments", repo, number)
	payload := map[string]string{"body": body}

	var comment Comment
	err := c.send(ctx, http.MethodPost, path, payload, &comment)
	return comment, err
}

// UpdateComment rewrites a comment this service posted before, so that a pull
// request shows one review rather than one per push.
func (c *Client) UpdateComment(ctx context.Context, repo string, commentID int64, body string) (Comment, error) {
	path := fmt.Sprintf("/repos/%s/issues/comments/%d", repo, commentID)
	payload := map[string]string{"body": body}

	var comment Comment
	err := c.send(ctx, http.MethodPatch, path, payload, &comment)
	return comment, err
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	return c.send(ctx, http.MethodGet, path, nil, out)
}

// getAll follows the Link header until there is no next page.
func getAll[T any](ctx context.Context, c *Client, path string) ([]T, error) {
	var all []T
	next := path
	for page := 0; next != "" && page < 100; page++ {
		response, err := c.do(ctx, http.MethodGet, next, nil)
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
		link := response.Header.Get("Link")
		_ = response.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("github: could not read %s: %w", next, err)
		}

		var items []T
		if err := json.Unmarshal(body, &items); err != nil {
			return nil, fmt.Errorf("github: could not decode the page at %s: %w", next, err)
		}
		all = append(all, items...)
		next = nextPage(link, c.BaseURL)
	}
	return all, nil
}

func (c *Client) send(ctx context.Context, method, path string, payload any, out any) error {
	response, err := c.do(ctx, method, path, payload)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	body, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("github: could not read the response: %w", err)
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("github: could not decode %s: %w", path, err)
		}
	}
	return nil
}

// do performs one request with retries, and turns a refusal into an APIError.
func (c *Client) do(ctx context.Context, method, path string, payload any) (*http.Response, error) {
	var encoded []byte
	if payload != nil {
		var err error
		encoded, err = json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("github: could not encode the request: %w", err)
		}
	}

	policy := c.Policy
	response, err := policy.Do(ctx, c.client(), func(ctx context.Context) (*http.Request, error) {
		var body io.Reader
		if encoded != nil {
			body = bytes.NewReader(encoded)
		}
		endpoint := path
		if !strings.HasPrefix(endpoint, "http") {
			endpoint = c.BaseURL + path
		}
		request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
		if err != nil {
			return nil, err
		}
		request.Header.Set("Accept", "application/vnd.github+json")
		request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		request.Header.Set("User-Agent", c.UserAgent)
		if encoded != nil {
			request.Header.Set("Content-Type", "application/json")
		}
		if c.Token != "" {
			request.Header.Set("Authorization", "Bearer "+c.Token)
		}
		return request, nil
	})
	if err != nil {
		return nil, err
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return response, nil
	}

	defer response.Body.Close()
	message, _ := io.ReadAll(io.LimitReader(response.Body, 8<<10))
	return nil, &APIError{
		Status:  response.StatusCode,
		Method:  method,
		Path:    path,
		Message: strings.TrimSpace(string(message)),
	}
}

func (c *Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

var linkPattern = regexp.MustCompile(`<([^>]+)>\s*;\s*rel="([^"]+)"`)

// nextPage reads the Link header and returns the URL of the next page, or an
// empty string when this was the last one.
func nextPage(header, baseURL string) string {
	for _, part := range strings.Split(header, ",") {
		match := linkPattern.FindStringSubmatch(strings.TrimSpace(part))
		if match == nil || match[2] != "next" {
			continue
		}
		target := match[1]
		// The link is absolute; keep it on the configured host so that a test
		// server is not sent to github.com by its own pagination.
		parsed, err := url.Parse(target)
		if err != nil {
			return ""
		}
		base, err := url.Parse(baseURL)
		if err != nil {
			return ""
		}
		if parsed.Host == "" {
			return target
		}
		if parsed.Host != base.Host {
			parsed.Scheme = base.Scheme
			parsed.Host = base.Host
		}
		return parsed.String()
	}
	return ""
}

// ParseRepo splits "owner/name" and reports whether it is well formed.
func ParseRepo(repo string) (string, string, error) {
	parts := strings.Split(strings.TrimSpace(repo), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("github: %q is not an owner/name repository", repo)
	}
	return parts[0], parts[1], nil
}

// ParseNumber reads a pull request number from a string.
func ParseNumber(value string) (int, error) {
	number, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || number <= 0 {
		return 0, fmt.Errorf("github: %q is not a pull request number", value)
	}
	return number, nil
}
