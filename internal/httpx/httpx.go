// Package httpx does the one thing every outbound call in this service needs:
// retry the failures that are worth retrying, give up on the ones that are not,
// and never wait for ever.
package httpx

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// Policy says how hard to try.
type Policy struct {
	// Attempts counts the first try. One means no retrying.
	Attempts int

	// BaseDelay is the wait before the second attempt; it doubles each time.
	BaseDelay time.Duration

	// MaxDelay caps that growth.
	MaxDelay time.Duration

	// Sleep waits, and exists so tests do not have to. It returns the
	// context's error if the wait was cut short.
	Sleep func(ctx context.Context, d time.Duration) error
}

// DefaultPolicy retries three times with an increasing delay, which is what the
// challenge asks for and what handles a rate limit in practice.
func DefaultPolicy() Policy {
	return Policy{
		Attempts:  3,
		BaseDelay: 200 * time.Millisecond,
		MaxDelay:  5 * time.Second,
		Sleep:     Sleep,
	}
}

// Sleep waits for d, or until the context is done.
func Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Request builds one attempt. It is called per attempt because a request body
// cannot be sent twice.
type Request func(ctx context.Context) (*http.Request, error)

// Do sends a request and retries the responses and errors that are worth
// retrying.
//
// A response that is not worth retrying is returned as it is, for the caller to
// interpret, and so is a retryable status on the last attempt: the status is
// the answer, and turning it into an error here would hide the body that
// explains it. The caller owns the body. An error is returned only when the
// attempts were exhausted by transport failures, or when the context was
// cancelled.
func (p Policy) Do(ctx context.Context, client *http.Client, build Request) (*http.Response, error) {
	attempts := p.Attempts
	if attempts <= 0 {
		attempts = 1
	}
	sleep := p.Sleep
	if sleep == nil {
		sleep = Sleep
	}

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		request, err := build(ctx)
		if err != nil {
			return nil, fmt.Errorf("could not build the request: %w", err)
		}

		response, err := client.Do(request)
		if err != nil {
			// A cancelled context is not a failure to retry: the caller asked
			// to stop.
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = err
			if attempt == attempts {
				break
			}
			if waitErr := sleep(ctx, p.delay(attempt, 0)); waitErr != nil {
				return nil, waitErr
			}
			continue
		}

		if !Retryable(response.StatusCode) || attempt == attempts {
			return response, nil
		}

		// The body of a response that is about to be retried has to be drained
		// and closed, or the connection cannot be reused.
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		_ = response.Body.Close()

		if waitErr := sleep(ctx, p.delay(attempt, RetryAfter(response))); waitErr != nil {
			return nil, waitErr
		}
	}
	return nil, fmt.Errorf("gave up after %d attempt(s): %w", attempts, lastErr)
}

// delay is the wait before the next attempt: the server's own advice when it
// gives any, and otherwise an exponential backoff.
func (p Policy) delay(attempt int, serverSays time.Duration) time.Duration {
	if serverSays > 0 {
		if serverSays > p.MaxDelay && p.MaxDelay > 0 {
			return p.MaxDelay
		}
		return serverSays
	}
	delay := p.BaseDelay
	for i := 1; i < attempt; i++ {
		delay *= 2
		if p.MaxDelay > 0 && delay >= p.MaxDelay {
			return p.MaxDelay
		}
	}
	return delay
}

// Retryable reports whether a status is worth another attempt. A rate limit and
// the server-side failures are; a bad request or a missing repository is not.
func Retryable(status int) bool {
	switch status {
	case http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

// RetryAfter reads the header a rate limiting server uses to say when to come
// back. It is either a number of seconds or an HTTP date; both are allowed.
func RetryAfter(response *http.Response) time.Duration {
	value := response.Header.Get("Retry-After")
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		wait := time.Until(when)
		if wait <= 0 {
			return 0
		}
		return wait
	}
	return 0
}
