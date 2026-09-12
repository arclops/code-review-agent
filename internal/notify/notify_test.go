package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arclops/code-review-agent/internal/httpx"
)

// sleepLog records the waits a notifier's policy asked for instead of sleeping.
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

func notifierForTest(server *httptest.Server, attempts int) (*Notifier, *sleepLog) {
	log := &sleepLog{}
	notifier := New(server.URL, 5*time.Second)
	notifier.Client = server.Client()
	notifier.Policy = httpx.Policy{Attempts: attempts, Sleep: log.sleep}
	return notifier, log
}

type webhook struct {
	mutex  sync.Mutex
	posts  int
	bodies []string
}

func (w *webhook) record(request *http.Request) int {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		body = []byte("could not read the body: " + err.Error())
	}
	w.mutex.Lock()
	defer w.mutex.Unlock()
	w.posts++
	w.bodies = append(w.bodies, string(body))
	return w.posts
}

func (w *webhook) count() int {
	w.mutex.Lock()
	defer w.mutex.Unlock()
	return w.posts
}

func (w *webhook) recorded() []string {
	w.mutex.Lock()
	defer w.mutex.Unlock()
	return append([]string(nil), w.bodies...)
}

func TestEnabledIsFalseWithoutAURLAndTheNilNotifierIsSafe(t *testing.T) {
	if notifier := New("", time.Second); notifier.Enabled() {
		t.Error("Enabled() = true for an empty URL, want false")
	}
	zero := &Notifier{}
	if zero.Enabled() {
		t.Error("Enabled() = true for a zero notifier, want false")
	}

	var nilNotifier *Notifier
	if nilNotifier.Enabled() {
		t.Error("Enabled() = true on a nil notifier, want false")
	}
	if err := nilNotifier.Notify(context.Background(), "review failed"); err != nil {
		t.Errorf("Notify() on a nil notifier = %v, want nil", err)
	}

	disabled := New("", time.Second)
	if err := disabled.Notify(context.Background(), "review failed"); err != nil {
		t.Errorf("Notify() without a URL = %v, want nil", err)
	}
	if disabled.Notify(context.Background(), "review failed") != nil {
		t.Error("Notify() without a URL should stay a no-op")
	}
}

func TestNewAppliesTheDocumentedDefaults(t *testing.T) {
	cases := []struct {
		name    string
		timeout time.Duration
		want    time.Duration
	}{
		{name: "zero becomes ten seconds", timeout: 0, want: 10 * time.Second},
		{name: "negative becomes ten seconds", timeout: -time.Second, want: 10 * time.Second},
		{name: "a positive timeout is kept", timeout: 3 * time.Second, want: 3 * time.Second},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			notifier := New("https://hooks.example.test/services/abc", testCase.timeout)
			if notifier.Timeout != testCase.want {
				t.Errorf("Timeout = %v, want %v", notifier.Timeout, testCase.want)
			}
			if notifier.URL == "" {
				t.Error("URL is empty, want the configured webhook")
			}
			if !notifier.Enabled() {
				t.Error("Enabled() = false, want true for a configured webhook")
			}
			if notifier.Client == nil {
				t.Error("Client = nil, want a client")
			}
			if notifier.Name != "code-review-agent" {
				t.Errorf("Name = %q, want code-review-agent", notifier.Name)
			}
			if notifier.Policy.Attempts != 3 {
				t.Errorf("Policy.Attempts = %d, want the default policy", notifier.Policy.Attempts)
			}
		})
	}
}

func TestNotifySendsTheMessageUnderBothSlackAndDiscordKeys(t *testing.T) {
	hook := &webhook{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hook.record(r)

		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		if got := r.Header.Get("User-Agent"); got != "code-review-agent" {
			t.Errorf("User-Agent = %q, want code-review-agent", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	notifier, sleeps := notifierForTest(server, 3)
	const message = "review failed for acme/widget#7: the model returned no JSON"

	if err := notifier.Notify(context.Background(), message); err != nil {
		t.Fatalf("Notify() = %v, want nil", err)
	}
	if hook.count() != 1 {
		t.Fatalf("posts = %d, want exactly one", hook.count())
	}

	bodies := hook.recorded()
	var payload map[string]string
	if err := json.Unmarshal([]byte(bodies[0]), &payload); err != nil {
		t.Fatalf("the payload %s is not JSON: %v", bodies[0], err)
	}
	if payload["text"] != message {
		t.Errorf("payload[text] = %q, want the message for Slack", payload["text"])
	}
	if payload["content"] != message {
		t.Errorf("payload[content] = %q, want the message for Discord", payload["content"])
	}
	if payload["username"] != "code-review-agent" {
		t.Errorf("payload[username] = %q, want the notifier name", payload["username"])
	}
	if !strings.Contains(bodies[0], `"text"`) || !strings.Contains(bodies[0], `"content"`) {
		t.Errorf("payload = %s, want both the text and content keys in the body", bodies[0])
	}
	if sleeps.count() != 0 {
		t.Errorf("waits = %d, want none on the first success", sleeps.count())
	}
}

func TestNotifyRetriesServerErrorsButNotClientErrors(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		wantPosts  int
		wantWaits  int
		wantErrSub string
	}{
		{
			name:       "a 500 is retried and then reported",
			status:     http.StatusInternalServerError,
			wantPosts:  3,
			wantWaits:  2,
			wantErrSub: "the webhook answered 500 Internal Server Error",
		},
		{
			name:       "a 503 is retried and then reported",
			status:     http.StatusServiceUnavailable,
			wantPosts:  3,
			wantWaits:  2,
			wantErrSub: "the webhook answered 503 Service Unavailable",
		},
		{
			name:       "a 400 is not retried",
			status:     http.StatusBadRequest,
			wantPosts:  1,
			wantWaits:  0,
			wantErrSub: "the webhook answered 400 Bad Request",
		},
		{
			name:       "a 404 is not retried",
			status:     http.StatusNotFound,
			wantPosts:  1,
			wantWaits:  0,
			wantErrSub: "the webhook answered 404 Not Found",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			hook := &webhook{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hook.record(r)
				w.WriteHeader(testCase.status)
				_, _ = io.WriteString(w, "the webhook said no")
			}))
			defer server.Close()

			notifier, sleeps := notifierForTest(server, 3)
			err := notifier.Notify(context.Background(), "review failed")
			if err == nil {
				t.Fatalf("Notify() = nil, want an error for status %d", testCase.status)
			}
			if !strings.HasPrefix(err.Error(), "notify: ") {
				t.Errorf("error = %q, want it prefixed with the package name", err)
			}
			if !strings.Contains(err.Error(), testCase.wantErrSub) {
				t.Errorf("error = %q, want it to contain %q", err, testCase.wantErrSub)
			}
			if hook.count() != testCase.wantPosts {
				t.Errorf("posts = %d, want %d", hook.count(), testCase.wantPosts)
			}
			if sleeps.count() != testCase.wantWaits {
				t.Errorf("waits = %d, want %d", sleeps.count(), testCase.wantWaits)
			}
		})
	}
}

func TestNotifyRetriesTheSameMessageAndStopsOnSuccess(t *testing.T) {
	hook := &webhook{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := hook.record(r)
		if attempt == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	notifier, sleeps := notifierForTest(server, 3)
	const message = "review failed for acme/widget#7"

	if err := notifier.Notify(context.Background(), message); err != nil {
		t.Fatalf("Notify() = %v, want nil after the retry", err)
	}
	if hook.count() != 2 {
		t.Fatalf("posts = %d, want 2: the retry posts once more", hook.count())
	}
	if sleeps.count() != 1 {
		t.Errorf("waits = %d, want 1", sleeps.count())
	}

	bodies := hook.recorded()
	if bodies[0] != bodies[1] {
		t.Errorf("payloads differ:\n%s\n%s\nwant the same body replayed on the retry", bodies[0], bodies[1])
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(bodies[1]), &payload); err != nil {
		t.Fatalf("the payload %s is not JSON: %v", bodies[1], err)
	}
	if payload["text"] != message {
		t.Errorf("payload[text] = %q, want the message", payload["text"])
	}
}

func TestNotifyAcceptsEveryTwoHundredStatus(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusCreated, http.StatusAccepted, http.StatusNoContent} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
			}))
			defer server.Close()

			notifier, _ := notifierForTest(server, 3)
			if err := notifier.Notify(context.Background(), "review failed"); err != nil {
				t.Errorf("Notify() = %v, want nil for status %d", err, status)
			}
		})
	}
}

func TestNotifyStopsWhenTheWaitIsCancelled(t *testing.T) {
	hook := &webhook{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hook.record(r)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	notifier := New(server.URL, 5*time.Second)
	notifier.Client = server.Client()
	notifier.Policy = httpx.Policy{
		Attempts: 3,
		Sleep: func(context.Context, time.Duration) error {
			return context.Canceled
		},
	}

	err := notifier.Notify(context.Background(), "review failed")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Notify() = %v, want the cancelled wait's error", err)
	}
	if hook.count() != 1 {
		t.Errorf("posts = %d, want 1: a cancelled wait must stop the retries", hook.count())
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestNotifyUsesTheInjectedClient(t *testing.T) {
	hook := &webhook{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hook.record(r)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	var used int
	notifier := New(server.URL, 5*time.Second)
	notifier.Client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		used++
		return server.Client().Transport.RoundTrip(request)
	})}
	notifier.Policy = httpx.Policy{Attempts: 1, Sleep: func(context.Context, time.Duration) error { return nil }}

	if err := notifier.Notify(context.Background(), "review failed"); err != nil {
		t.Fatalf("Notify() = %v, want nil", err)
	}
	if used != 1 {
		t.Errorf("the injected client was used %d times, want 1", used)
	}
	if hook.count() != 1 {
		t.Errorf("posts = %d, want 1", hook.count())
	}
}
