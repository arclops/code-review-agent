package webhook

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/arclops/code-review-agent/internal/runner"
)

const testSecret = "a-shared-secret-that-only-github-and-the-server-know"

// capture is a queue stand-in: the handler only ever submits.
type capture struct {
	mu   sync.Mutex
	jobs []runner.Job
}

func newQueue(t *testing.T, capture *capture) *runner.Queue {
	t.Helper()
	return runner.New(runner.Options{
		Logger: log.New(io.Discard, "", 0),
		Reviewer: func(_ context.Context, job runner.Job) (runner.Outcome, error) {
			capture.mu.Lock()
			capture.jobs = append(capture.jobs, job)
			capture.mu.Unlock()
			return runner.Outcome{Posted: true}, nil
		},
	})
}

func (c *capture) submitted() []runner.Job {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]runner.Job(nil), c.jobs...)
}

func payload(action, repo string, number int) []byte {
	body := map[string]any{
		"action": action,
		"number": number,
		"repository": map[string]any{
			"full_name": repo,
		},
		"pull_request": map[string]any{
			"number": number,
			"draft":  false,
			"head":   map[string]any{"sha": "0123456789abcdef0123456789abcdef01234567"},
		},
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return encoded
}

func newHandler(queue *runner.Queue) *Handler {
	return New(Options{Secret: testSecret, Queue: queue, Logger: log.New(io.Discard, "", 0)})
}

func post(t *testing.T, handler *Handler, event string, body []byte, signature string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/webhook/github", strings.NewReader(string(body)))
	request.Header.Set("X-GitHub-Event", event)
	if signature != "" {
		request.Header.Set(SignatureHeader, signature)
	}
	recorder := httptest.NewRecorder()
	handler.Routes().ServeHTTP(recorder, request)
	return recorder
}

// ------------------------------------------------------------------ signatures

func TestVerifyAcceptsTheSignatureGitHubSends(t *testing.T) {
	body := payload("opened", "acme/widget", 1)
	if !Verify(testSecret, body, Sign(testSecret, body)) {
		t.Error("a signature this package produced was rejected")
	}
	if !strings.HasPrefix(Sign(testSecret, body), "sha256=") {
		t.Errorf("Sign returned %q, want the sha256= prefix GitHub sends", Sign(testSecret, body))
	}
}

func TestVerifyRejectsEverythingElse(t *testing.T) {
	body := payload("opened", "acme/widget", 1)
	good := Sign(testSecret, body)

	cases := []struct {
		name    string
		secret  string
		body    []byte
		header  string
		wantLog string
	}{
		{"a different secret", "not-the-secret", body, good, ""},
		{"a tampered body", testSecret, append(body, ' '), good, ""},
		{"no header at all", testSecret, body, "", ""},
		{"a header without the prefix", testSecret, body, strings.TrimPrefix(good, "sha256="), ""},
		{"a header that is not hex", testSecret, body, "sha256=zzzz", ""},
		{"no secret configured", "", body, good, ""},
	}
	for _, tc := range cases {
		if Verify(tc.secret, tc.body, tc.header) {
			t.Errorf("%s: Verify returned true, want false", tc.name)
		}
	}
}

func TestWebhookRefusesADeliveryWithABadSignature(t *testing.T) {
	capture := &capture{}
	queue := newQueue(t, capture)
	defer queue.Wait()
	handler := newHandler(queue)

	recorder := post(t, handler, "pull_request", payload("opened", "acme/widget", 1), "sha256=deadbeef")
	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", recorder.Code)
	}
	if len(capture.submitted()) != 0 {
		t.Error("an unsigned delivery started a review")
	}
}

func TestWebhookRefusesEverythingWhenNoSecretIsConfigured(t *testing.T) {
	capture := &capture{}
	queue := newQueue(t, capture)
	defer queue.Wait()
	handler := New(Options{Queue: queue, Logger: log.New(io.Discard, "", 0)})

	body := payload("opened", "acme/widget", 1)
	recorder := post(t, handler, "pull_request", body, Sign("", body))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503: without a secret the server cannot tell a real delivery from anybody else's", recorder.Code)
	}
	if len(capture.submitted()) != 0 {
		t.Error("a delivery was accepted although no secret is configured")
	}
}

// ---------------------------------------------------------------- event handling

func TestWebhookQueuesTheActionsThatChangeCode(t *testing.T) {
	for _, action := range DefaultActions {
		capture := &capture{}
		queue := newQueue(t, capture)
		handler := newHandler(queue)

		body := payload(action, "acme/widget", 12)
		recorder := post(t, handler, "pull_request", body, Sign(testSecret, body))
		if recorder.Code != http.StatusAccepted {
			t.Errorf("%s: status = %d, want 202", action, recorder.Code)
		}
		queue.Wait()

		jobs := capture.submitted()
		if len(jobs) != 1 {
			t.Fatalf("%s: %d review(s) started, want one", action, len(jobs))
		}
		if jobs[0].Repository != "acme/widget" || jobs[0].Number != 12 {
			t.Errorf("%s: queued %+v, want acme/widget#12", action, jobs[0])
		}
		if jobs[0].Event != action {
			t.Errorf("%s: the job records event %q", action, jobs[0].Event)
		}
		if jobs[0].HeadSHA != "0123456789abcdef0123456789abcdef01234567" {
			t.Errorf("%s: the job does not carry the head revision", action)
		}

		var response runner.Record
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatalf("%s: the response is not a run record: %v", action, err)
		}
		if response.ID == "" || response.State != runner.StateQueued {
			t.Errorf("%s: the response is %+v, want a queued run with an id", action, response)
		}
	}
}

func TestWebhookIgnoresTheEventsThatDoNotChangeCode(t *testing.T) {
	cases := []struct {
		event  string
		body   []byte
		reason string
	}{
		{"pull_request", payload("labeled", "acme/widget", 3), "labeled"},
		{"pull_request", payload("closed", "acme/widget", 3), "closed"},
		{"pull_request", payload("edited", "acme/widget", 3), "edited"},
		{"issues", payload("opened", "acme/widget", 3), "issues"},
		{"push", []byte(`{"ref":"refs/heads/main"}`), "push"},
		{"ping", []byte(`{"zen":"Keep it logically awesome."}`), "ping"},
	}
	for _, tc := range cases {
		capture := &capture{}
		queue := newQueue(t, capture)
		handler := newHandler(queue)

		recorder := post(t, handler, tc.event, tc.body, Sign(testSecret, tc.body))
		// 202, not an error: GitHub retries a delivery it thinks failed, and
		// there is nothing here to retry.
		if recorder.Code != http.StatusAccepted {
			t.Errorf("%s: status = %d, want 202", tc.event, recorder.Code)
		}
		if !strings.Contains(recorder.Body.String(), "ignored") {
			t.Errorf("%s: the response does not say it was ignored: %s", tc.event, recorder.Body.String())
		}
		queue.Wait()
		if len(capture.submitted()) != 0 {
			t.Errorf("%s: a review was started for an event that changes nothing (%s)", tc.event, tc.reason)
		}
	}
}

func TestWebhookRejectsAPayloadThatNamesNoPullRequest(t *testing.T) {
	capture := &capture{}
	queue := newQueue(t, capture)
	handler := newHandler(queue)

	body := []byte(`{"action":"opened","repository":{"full_name":""},"pull_request":{"number":0}}`)
	recorder := post(t, handler, "pull_request", body, Sign(testSecret, body))
	if recorder.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", recorder.Code)
	}
	queue.Wait()
	if len(capture.submitted()) != 0 {
		t.Error("a review was started for a payload with no repository or number")
	}
}

func TestWebhookRejectsAPayloadThatIsNotJSON(t *testing.T) {
	capture := &capture{}
	queue := newQueue(t, capture)
	handler := newHandler(queue)
	body := []byte("this is not JSON")

	recorder := post(t, handler, "pull_request", body, Sign(testSecret, body))
	if recorder.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", recorder.Code)
	}
	queue.Wait()
}

func TestWebhookRejectsOtherMethods(t *testing.T) {
	capture := &capture{}
	queue := newQueue(t, capture)
	handler := newHandler(queue)

	request := httptest.NewRequest(http.MethodGet, "/webhook/github", nil)
	recorder := httptest.NewRecorder()
	handler.Routes().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", recorder.Code)
	}
	queue.Wait()
}

func TestWebhookRejectsAnOversizedBody(t *testing.T) {
	capture := &capture{}
	queue := newQueue(t, capture)
	handler := New(Options{Secret: testSecret, Queue: queue, Logger: log.New(io.Discard, "", 0), MaxBody: 64})

	body := payload("opened", "acme/widget", 1)
	recorder := post(t, handler, "pull_request", body, Sign(testSecret, body))
	if recorder.Code == http.StatusAccepted {
		t.Error("a body over the limit was accepted")
	}
	queue.Wait()
	if len(capture.submitted()) != 0 {
		t.Error("a truncated body started a review")
	}
}

// ------------------------------------------------------------------- the routes

func TestRunsEndpointReportsTheHistory(t *testing.T) {
	capture := &capture{}
	queue := newQueue(t, capture)
	handler := newHandler(queue)

	body := payload("opened", "acme/widget", 5)
	post(t, handler, "pull_request", body, Sign(testSecret, body))
	queue.Wait()

	recorder := httptest.NewRecorder()
	handler.Routes().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/runs", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}

	var payload struct {
		Runs []runner.Record `json:"runs"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("the response is not a run list: %v", err)
	}
	if len(payload.Runs) != 1 {
		t.Fatalf("the history has %d run(s), want 1", len(payload.Runs))
	}
	if payload.Runs[0].State != runner.StateSucceeded || !payload.Runs[0].Posted {
		t.Errorf("the recorded run is %+v, want a succeeded posted review", payload.Runs[0])
	}

	id := payload.Runs[0].ID
	one := httptest.NewRecorder()
	handler.Routes().ServeHTTP(one, httptest.NewRequest(http.MethodGet, "/runs/"+id, nil))
	if one.Code != http.StatusOK {
		t.Fatalf("GET /runs/%s = %d, want 200", id, one.Code)
	}
	var record runner.Record
	if err := json.Unmarshal(one.Body.Bytes(), &record); err != nil {
		t.Fatalf("GET /runs/%s did not answer with a record: %v", id, err)
	}
	if record.ID != id {
		t.Errorf("GET /runs/%s returned the record for %q", id, record.ID)
	}

	missing := httptest.NewRecorder()
	handler.Routes().ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/runs/nope", nil))
	if missing.Code != http.StatusNotFound {
		t.Errorf("GET /runs/nope = %d, want 404", missing.Code)
	}
}

func TestHealthzReportsTheActiveRuns(t *testing.T) {
	capture := &capture{}
	queue := newQueue(t, capture)
	handler := newHandler(queue)

	recorder := httptest.NewRecorder()
	handler.Routes().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("the health response is not JSON: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("health = %v, want status ok", body)
	}
	queue.Wait()
}
