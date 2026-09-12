// Package notify tells somebody that a review failed.
//
// Without it, a broken review and a pull request nobody reviewed look exactly
// the same, and that is the difference the challenge draws between a system you
// trust and a system you have to check on.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/arclops/code-review-agent/internal/httpx"
)

// Notifier posts a message to a webhook.
//
// The payload carries the message under both the key Slack uses and the key
// Discord uses, and each service ignores the key it does not know, so one
// setting works for either.
type Notifier struct {
	URL     string
	Timeout time.Duration
	Client  *http.Client
	Policy  httpx.Policy
	Name    string
}

// New builds a notifier. An empty URL makes every call a no-op, which is what a
// deployment that has not configured one wants.
func New(url string, timeout time.Duration) *Notifier {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Notifier{
		URL:     url,
		Timeout: timeout,
		Client:  &http.Client{},
		Policy:  httpx.DefaultPolicy(),
		Name:    "code-review-agent",
	}
}

// Enabled reports whether there is anywhere to send to.
func (n *Notifier) Enabled() bool { return n != nil && n.URL != "" }

// Notify posts a message.
func (n *Notifier) Notify(ctx context.Context, message string) error {
	if !n.Enabled() {
		return nil
	}
	if n.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, n.Timeout)
		defer cancel()
	}

	payload, err := json.Marshal(map[string]string{
		"text":     message,
		"content":  message,
		"username": n.Name,
	})
	if err != nil {
		return err
	}

	response, err := n.Policy.Do(ctx, n.client(), func(ctx context.Context) (*http.Request, error) {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, n.URL, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("User-Agent", n.Name)
		return request, nil
	})
	if err != nil {
		return fmt.Errorf("notify: %w", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("notify: the webhook answered %s", response.Status)
	}
	return nil
}

func (n *Notifier) client() *http.Client {
	if n.Client != nil {
		return n.Client
	}
	return http.DefaultClient
}
