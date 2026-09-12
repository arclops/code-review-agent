// Package webhook receives GitHub's events and hands the interesting ones to
// the run queue.
//
// Two things are load bearing here. The signature check, because an
// unauthenticated endpoint that starts reviews is an endpoint anybody can use
// to spend somebody else's money. And the event filter, because GitHub sends
// everything: somebody adding a label must not start a code review.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/arclops/code-review-agent/internal/runner"
)

// SignatureHeader is where GitHub puts the HMAC of the body.
const SignatureHeader = "X-Hub-Signature-256"

// DefaultActions are the pull request actions that change code. Everything else
// is somebody labelling, assigning, or commenting.
var DefaultActions = []string{"opened", "reopened", "synchronize"}

// Options configure the handler.
type Options struct {
	// Secret is the webhook secret. With no secret the endpoint refuses
	// everything, because failing closed is the only safe default.
	Secret string

	// Actions are the pull request actions that start a review.
	Actions []string

	Queue  *runner.Queue
	Logger *log.Logger

	// MaxBody bounds the request body.
	MaxBody int64
}

// Handler serves the webhook and the run history.
type Handler struct {
	options Options
}

// New builds a handler.
func New(options Options) *Handler {
	if options.Logger == nil {
		options.Logger = log.New(io.Discard, "", 0)
	}
	if len(options.Actions) == 0 {
		options.Actions = DefaultActions
	}
	if options.MaxBody <= 0 {
		options.MaxBody = 5 << 20
	}
	return &Handler{options: options}
}

// Routes wires up the endpoints.
func (h *Handler) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/webhook/github", h.serveWebhook)
	mux.HandleFunc("/runs", h.serveRuns)
	mux.HandleFunc("/runs/", h.serveRun)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "active_runs": h.options.Queue.Active()})
	})
	return mux
}

// Event is the part of a pull request event this service reads.
type Event struct {
	Action     string `json:"action"`
	Number     int    `json:"number"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	PullRequest struct {
		Number int  `json:"number"`
		Draft  bool `json:"draft"`
		Head   struct {
			SHA string `json:"sha"`
		} `json:"head"`
	} `json:"pull_request"`
}

func (h *Handler) serveWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "the webhook is a POST endpoint"})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, h.options.MaxBody))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "could not read the body"})
		return
	}

	if h.options.Secret == "" {
		h.options.Logger.Printf("refused a delivery: no webhook secret is configured")
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "this server has no webhook secret configured, so it cannot tell a real delivery from anybody else's request",
		})
		return
	}
	if !Verify(h.options.Secret, body, r.Header.Get(SignatureHeader)) {
		h.options.Logger.Printf("refused a delivery with a bad or missing %s header", SignatureHeader)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "the signature does not match"})
		return
	}

	eventName := r.Header.Get("X-GitHub-Event")
	if eventName != "pull_request" {
		// Anything else is answered 202 rather than an error: GitHub retries a
		// delivery it thinks failed, and there is nothing to retry here.
		h.ignore(w, fmt.Sprintf("the %q event is not a pull request event", eventName))
		return
	}

	var event Event
	if err := json.Unmarshal(body, &event); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the payload is not the JSON expected"})
		return
	}

	if !contains(h.options.Actions, event.Action) {
		h.ignore(w, fmt.Sprintf("the %q action does not change the code", event.Action))
		return
	}
	if event.Repository.FullName == "" || event.PullRequest.Number == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the payload names no repository or pull request"})
		return
	}

	record := h.options.Queue.Submit(r.Context(), runner.Job{
		Repository: event.Repository.FullName,
		Number:     event.PullRequest.Number,
		Event:      event.Action,
		HeadSHA:    event.PullRequest.Head.SHA,
	})
	h.options.Logger.Printf("queued run %s for %s#%d (%s)", record.ID, record.Repository, record.Number, event.Action)
	writeJSON(w, http.StatusAccepted, record)
}

func (h *Handler) ignore(w http.ResponseWriter, reason string) {
	h.options.Logger.Printf("ignored a delivery: %s", reason)
	writeJSON(w, http.StatusAccepted, map[string]any{"ignored": reason})
}

func (h *Handler) serveRuns(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"runs": h.options.Queue.List()})
}

func (h *Handler) serveRun(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/runs/")
	if id == "" {
		h.serveRuns(w, r)
		return
	}
	record, ok := h.options.Queue.Get(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no run with that id"})
		return
	}
	writeJSON(w, http.StatusOK, record)
}

// Verify checks the signature GitHub sent against the body.
//
// The comparison is constant time: a byte by byte comparison that returns early
// leaks how much of the signature was right, which is enough to forge one a
// byte at a time.
func Verify(secret string, body []byte, header string) bool {
	const prefix = "sha256="
	if secret == "" || !strings.HasPrefix(header, prefix) {
		return false
	}
	provided, err := hex.DecodeString(strings.TrimPrefix(header, prefix))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), provided)
}

// Sign produces the header value for a body. It exists for the tests and for
// anything that wants to replay a delivery.
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
