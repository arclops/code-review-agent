package httpx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// roundTripFunc stands in for the network without a server.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

// waitRecorder records the waits a policy asked for instead of sleeping, so no
// test ever pays the backoff.
type waitRecorder struct {
	mutex sync.Mutex
	waits []time.Duration
}

func (r *waitRecorder) sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.waits = append(r.waits, d)
	return nil
}

func (r *waitRecorder) recorded() []time.Duration {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	return append([]time.Duration(nil), r.waits...)
}

func recordingPolicy(attempts int, base, max time.Duration) (Policy, *waitRecorder) {
	recorder := &waitRecorder{}
	return Policy{
		Attempts:  attempts,
		BaseDelay: base,
		MaxDelay:  max,
		Sleep:     recorder.sleep,
	}, recorder
}

func getRequest(url string) Request {
	return func(ctx context.Context) (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	}
}

func assertWaits(t *testing.T, recorder *waitRecorder, want ...time.Duration) {
	t.Helper()
	got := recorder.recorded()
	if len(got) != len(want) {
		t.Fatalf("waits = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Errorf("wait %d = %v, want %v", index, got[index], want[index])
		}
	}
}

// countingBody reports how often a response body was read and closed.
type countingBody struct {
	inner  io.ReadCloser
	mutex  *sync.Mutex
	closes *int
	read   *int
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.inner.Read(p)
	if n > 0 {
		b.mutex.Lock()
		*b.read += n
		b.mutex.Unlock()
	}
	return n, err
}

func (b *countingBody) Close() error {
	b.mutex.Lock()
	*b.closes++
	b.mutex.Unlock()
	return b.inner.Close()
}

func TestDoReturnsTheFirstSuccessWithoutWaiting(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	policy, recorder := recordingPolicy(3, 100*time.Millisecond, time.Second)
	response, err := policy.Do(context.Background(), server.Client(), getRequest(server.URL))
	if err != nil {
		t.Fatalf("Do returned an error on the first success: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	if calls != 1 {
		t.Errorf("requests = %d, want 1", calls)
	}
	assertWaits(t, recorder)
}

func TestDoReturnsTheResponseOfANonRetryableStatus(t *testing.T) {
	for _, status := range []int{
		http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusNotFound,
		http.StatusConflict,
		http.StatusUnprocessableEntity,
		http.StatusTeapot,
	} {
		t.Run(fmt.Sprintf("status_%d", status), func(t *testing.T) {
			var calls int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.WriteHeader(status)
			}))
			defer server.Close()

			policy, recorder := recordingPolicy(3, 100*time.Millisecond, time.Second)
			response, err := policy.Do(context.Background(), server.Client(), getRequest(server.URL))
			if err != nil {
				t.Fatalf("Do returned an error on status %d, want the response: %v", status, err)
			}
			defer response.Body.Close()

			if response.StatusCode != status {
				t.Errorf("status = %d, want %d", response.StatusCode, status)
			}
			if calls != 1 {
				t.Errorf("requests = %d, want 1: a %d must not be retried", calls, status)
			}
			assertWaits(t, recorder)
		})
	}
}

func TestDoRetriesRetryableStatusesUntilAttemptsRunOut(t *testing.T) {
	for _, status := range []int{
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
	} {
		t.Run(fmt.Sprintf("status_%d", status), func(t *testing.T) {
			var calls int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.WriteHeader(status)
			}))
			defer server.Close()

			policy, recorder := recordingPolicy(3, 100*time.Millisecond, time.Second)
			response, err := policy.Do(context.Background(), server.Client(), getRequest(server.URL))
			if err != nil {
				t.Fatalf("Do returned an error, want the last response: %v", err)
			}
			defer response.Body.Close()

			if calls != 3 {
				t.Errorf("requests = %d, want 3", calls)
			}
			if response.StatusCode != status {
				t.Errorf("status = %d, want the last %d returned to the caller", response.StatusCode, status)
			}
			// The first wait is BaseDelay, the second doubles it.
			assertWaits(t, recorder, 100*time.Millisecond, 200*time.Millisecond)
		})
	}
}

func TestDoRetriesRetryableStatusesUntilOneSucceeds(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	policy, recorder := recordingPolicy(3, 50*time.Millisecond, time.Second)
	response, err := policy.Do(context.Background(), server.Client(), getRequest(server.URL))
	if err != nil {
		t.Fatalf("Do returned an error after a successful retry: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	if calls != 3 {
		t.Errorf("requests = %d, want 3", calls)
	}
	assertWaits(t, recorder, 50*time.Millisecond, 100*time.Millisecond)
}

func TestDoCapsTheBackoffAtMaxDelay(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	policy, recorder := recordingPolicy(4, 100*time.Millisecond, 150*time.Millisecond)
	response, err := policy.Do(context.Background(), server.Client(), getRequest(server.URL))
	if err != nil {
		t.Fatalf("Do returned an error, want the last response: %v", err)
	}
	defer response.Body.Close()

	if calls != 4 {
		t.Errorf("requests = %d, want 4", calls)
	}
	assertWaits(t, recorder, 100*time.Millisecond, 150*time.Millisecond, 150*time.Millisecond)
}

func TestDoReturnsTheLastTransportErrorAfterExhaustion(t *testing.T) {
	refused := errors.New("connection refused")
	var calls int
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, refused
	})}

	policy, recorder := recordingPolicy(3, 20*time.Millisecond, time.Second)
	response, err := policy.Do(context.Background(), client, getRequest("http://example.invalid/"))
	if response != nil {
		t.Fatalf("response = %v, want nil", response)
	}
	if err == nil {
		t.Fatal("Do returned no error after every attempt failed")
	}
	if !errors.Is(err, refused) {
		t.Errorf("error = %v, want it to wrap the transport error", err)
	}
	if !strings.Contains(err.Error(), "gave up after 3 attempt(s)") {
		t.Errorf("error = %q, want it to name the number of attempts", err)
	}
	if calls != 3 {
		t.Errorf("requests = %d, want 3", calls)
	}
	assertWaits(t, recorder, 20*time.Millisecond, 40*time.Millisecond)
}

func TestDoRetriesATransportErrorThatLaterSucceeds(t *testing.T) {
	refused := errors.New("temporary failure")
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return nil, refused
		}
		return server.Client().Transport.RoundTrip(request)
	})}

	policy, recorder := recordingPolicy(3, 10*time.Millisecond, time.Second)
	response, err := policy.Do(context.Background(), client, getRequest(server.URL))
	if err != nil {
		t.Fatalf("Do returned an error after the retry succeeded: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	if calls != 2 {
		t.Errorf("requests = %d, want 2", calls)
	}
	assertWaits(t, recorder, 10*time.Millisecond)
}

func TestDoWithZeroAttemptsMakesExactlyOneAttempt(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	policy, recorder := recordingPolicy(0, 100*time.Millisecond, time.Second)
	response, err := policy.Do(context.Background(), server.Client(), getRequest(server.URL))
	if err != nil {
		t.Fatalf("Do returned an error: %v", err)
	}
	defer response.Body.Close()

	if calls != 1 {
		t.Errorf("requests = %d, want 1: a policy with no attempts is clamped to one", calls)
	}
	assertWaits(t, recorder)
}

func TestDoStopsWhenTheContextIsAlreadyCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("must not be reached twice")
	})}
	policy, recorder := recordingPolicy(5, time.Millisecond, time.Second)

	response, err := policy.Do(ctx, client, getRequest("http://example.invalid/"))
	if response != nil {
		t.Fatalf("response = %v, want nil", response)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
	assertWaits(t, recorder)
}

func TestDoReturnsTheWaitErrorAndStopsRetrying(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	policy := Policy{
		Attempts:  3,
		BaseDelay: time.Millisecond,
		Sleep: func(context.Context, time.Duration) error {
			return context.Canceled
		},
	}
	response, err := policy.Do(context.Background(), server.Client(), getRequest(server.URL))
	if response != nil {
		t.Fatalf("response = %v, want nil", response)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want the wait's error back", err)
	}
	if calls != 1 {
		t.Errorf("requests = %d, want 1: a cancelled wait must stop the retries", calls)
	}
}

func TestDoStopsWhenTheContextIsCancelledWhileWaiting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	policy := Policy{
		Attempts:  4,
		BaseDelay: time.Millisecond,
		Sleep: func(ctx context.Context, d time.Duration) error {
			cancel()
			return ctx.Err()
		},
	}
	response, err := policy.Do(ctx, server.Client(), getRequest(server.URL))
	if response != nil {
		t.Fatalf("response = %v, want nil", response)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
	if calls != 1 {
		t.Errorf("requests = %d, want 1: the request must not be sent after cancellation", calls)
	}
}

func TestDoReturnsABuildErrorWithoutSendingAnything(t *testing.T) {
	broken := errors.New("no URL")
	var calls int
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("must not be called")
	})}

	policy, recorder := recordingPolicy(3, time.Millisecond, time.Second)
	response, err := policy.Do(context.Background(), client, func(context.Context) (*http.Request, error) {
		return nil, broken
	})
	if response != nil {
		t.Fatalf("response = %v, want nil", response)
	}
	if !errors.Is(err, broken) {
		t.Errorf("error = %v, want it to wrap the build error", err)
	}
	if !strings.Contains(err.Error(), "could not build the request") {
		t.Errorf("error = %q, want it to say the request could not be built", err)
	}
	if calls != 0 {
		t.Errorf("requests = %d, want 0", calls)
	}
	assertWaits(t, recorder)
}

func TestDoReplaysTheRequestBodyOnEveryAttempt(t *testing.T) {
	const body = `{"prompt":"review this","n":1}`

	var mutex sync.Mutex
	var bodies []string
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading the request body: %v", err)
		}
		mutex.Lock()
		bodies = append(bodies, string(data))
		methods = append(methods, r.Method)
		attempt := len(bodies)
		mutex.Unlock()

		if attempt < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	policy, recorder := recordingPolicy(3, time.Millisecond, time.Second)
	response, err := policy.Do(context.Background(), server.Client(), func(ctx context.Context) (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodPost, server.URL, strings.NewReader(body))
	})
	if err != nil {
		t.Fatalf("Do returned an error: %v", err)
	}
	defer response.Body.Close()

	mutex.Lock()
	defer mutex.Unlock()
	if len(bodies) != 3 {
		t.Fatalf("requests = %d, want 3", len(bodies))
	}
	for index, got := range bodies {
		if got != body {
			t.Errorf("attempt %d sent %q, want %q: the body must be replayed", index+1, got, body)
		}
		if methods[index] != http.MethodPost {
			t.Errorf("attempt %d used %s, want POST", index+1, methods[index])
		}
	}
	assertWaits(t, recorder, time.Millisecond, 2*time.Millisecond)
}

func TestDoDrainsAndClosesTheBodyOfARetriedResponse(t *testing.T) {
	const body = "boom"
	var mutex sync.Mutex
	var closes, read int
	var calls int

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, body)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	base := server.Client().Transport
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		response, err := base.RoundTrip(request)
		if response != nil {
			response.Body = &countingBody{inner: response.Body, mutex: &mutex, closes: &closes, read: &read}
		}
		return response, err
	})}

	policy, _ := recordingPolicy(2, time.Millisecond, time.Second)
	response, err := policy.Do(context.Background(), client, getRequest(server.URL))
	if err != nil {
		t.Fatalf("Do returned an error: %v", err)
	}

	mutex.Lock()
	gotCloses, gotRead := closes, read
	mutex.Unlock()
	if gotCloses != 1 {
		t.Errorf("closes = %d, want 1: the retried response body must be closed", gotCloses)
	}
	if gotRead != len(body) {
		t.Errorf("bytes read = %d, want %d: the retried response body must be drained", gotRead, len(body))
	}

	_ = response.Body.Close()
	mutex.Lock()
	gotCloses = closes
	mutex.Unlock()
	if gotCloses != 2 {
		t.Errorf("closes = %d, want 2: the returned body belongs to the caller", gotCloses)
	}
}

func TestDoLeavesTheBodyOfANonRetryableResponseAlone(t *testing.T) {
	var mutex sync.Mutex
	var closes, read int

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "nope")
	}))
	defer server.Close()

	base := server.Client().Transport
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		response, err := base.RoundTrip(request)
		if response != nil {
			response.Body = &countingBody{inner: response.Body, mutex: &mutex, closes: &closes, read: &read}
		}
		return response, err
	})}

	policy, _ := recordingPolicy(3, time.Millisecond, time.Second)
	response, err := policy.Do(context.Background(), client, getRequest(server.URL))
	if err != nil {
		t.Fatalf("Do returned an error: %v", err)
	}
	defer response.Body.Close()

	mutex.Lock()
	defer mutex.Unlock()
	if closes != 0 {
		t.Errorf("closes = %d, want 0: the caller owns a body that is not retried", closes)
	}
	if read != 0 {
		t.Errorf("bytes read = %d, want 0: an untouched body must still be readable", read)
	}
}

func TestDoHonoursRetryAfterInSeconds(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	policy, recorder := recordingPolicy(3, 100*time.Millisecond, 5*time.Second)
	response, err := policy.Do(context.Background(), server.Client(), getRequest(server.URL))
	if err != nil {
		t.Fatalf("Do returned an error: %v", err)
	}
	defer response.Body.Close()

	if calls != 2 {
		t.Errorf("requests = %d, want 2", calls)
	}
	assertWaits(t, recorder, 2*time.Second)
}

func TestDoHonoursRetryAfterAsAnHTTPDate(t *testing.T) {
	newDateServer := func(t *testing.T) *httptest.Server {
		t.Helper()
		var calls int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			if calls == 1 {
				w.Header().Set("Retry-After", time.Now().Add(time.Hour).UTC().Format(http.TimeFormat))
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(server.Close)
		return server
	}

	t.Run("capped by max delay", func(t *testing.T) {
		server := newDateServer(t)
		policy, recorder := recordingPolicy(3, 100*time.Millisecond, 5*time.Second)
		response, err := policy.Do(context.Background(), server.Client(), getRequest(server.URL))
		if err != nil {
			t.Fatalf("Do returned an error: %v", err)
		}
		defer response.Body.Close()
		assertWaits(t, recorder, 5*time.Second)
	})

	t.Run("uncapped when max delay is zero", func(t *testing.T) {
		server := newDateServer(t)
		policy, recorder := recordingPolicy(3, 100*time.Millisecond, 0)
		response, err := policy.Do(context.Background(), server.Client(), getRequest(server.URL))
		if err != nil {
			t.Fatalf("Do returned an error: %v", err)
		}
		defer response.Body.Close()

		waits := recorder.recorded()
		if len(waits) != 1 {
			t.Fatalf("waits = %v, want exactly one wait", waits)
		}
		if waits[0] < 55*time.Minute || waits[0] > 65*time.Minute {
			t.Errorf("wait = %v, want roughly the hour the server asked for", waits[0])
		}
	})
}

func TestDoCapsRetryAfterAtMaxDelay(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "600")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	policy, recorder := recordingPolicy(3, 100*time.Millisecond, 5*time.Second)
	response, err := policy.Do(context.Background(), server.Client(), getRequest(server.URL))
	if err != nil {
		t.Fatalf("Do returned an error: %v", err)
	}
	defer response.Body.Close()

	assertWaits(t, recorder, 5*time.Second)
}

func TestRetryAfterReadsBothFormsAndRejectsNonsense(t *testing.T) {
	cases := []struct {
		name  string
		value string
		check func(t *testing.T, wait time.Duration)
	}{
		{
			name:  "seconds",
			value: "3",
			check: func(t *testing.T, wait time.Duration) {
				if wait != 3*time.Second {
					t.Errorf("wait = %v, want 3s", wait)
				}
			},
		},
		{
			name:  "zero seconds is no advice",
			value: "0",
			check: func(t *testing.T, wait time.Duration) {
				if wait != 0 {
					t.Errorf("wait = %v, want 0", wait)
				}
			},
		},
		{
			name:  "negative seconds is no advice",
			value: "-4",
			check: func(t *testing.T, wait time.Duration) {
				if wait != 0 {
					t.Errorf("wait = %v, want 0", wait)
				}
			},
		},
		{
			name:  "unparseable is no advice",
			value: "soon",
			check: func(t *testing.T, wait time.Duration) {
				if wait != 0 {
					t.Errorf("wait = %v, want 0", wait)
				}
			},
		},
		{
			name:  "a date in the past is no advice",
			value: time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat),
			check: func(t *testing.T, wait time.Duration) {
				if wait != 0 {
					t.Errorf("wait = %v, want 0", wait)
				}
			},
		},
		{
			name:  "a date in the future",
			value: time.Now().Add(30 * time.Minute).UTC().Format(http.TimeFormat),
			check: func(t *testing.T, wait time.Duration) {
				if wait < 25*time.Minute || wait > 35*time.Minute {
					t.Errorf("wait = %v, want roughly 30 minutes", wait)
				}
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			response := &http.Response{Header: http.Header{"Retry-After": []string{testCase.value}}}
			testCase.check(t, RetryAfter(response))
		})
	}

	t.Run("no header", func(t *testing.T) {
		if wait := RetryAfter(&http.Response{Header: http.Header{}}); wait != 0 {
			t.Errorf("wait = %v, want 0", wait)
		}
	})
}

func TestRetryableClassifiesStatuses(t *testing.T) {
	cases := map[int]bool{
		http.StatusTooManyRequests:     true,
		http.StatusInternalServerError: true,
		http.StatusBadGateway:          true,
		http.StatusServiceUnavailable:  true,
		http.StatusGatewayTimeout:      true,
		http.StatusOK:                  false,
		http.StatusCreated:             false,
		http.StatusNoContent:           false,
		http.StatusMovedPermanently:    false,
		http.StatusBadRequest:          false,
		http.StatusUnauthorized:        false,
		http.StatusForbidden:           false,
		http.StatusNotFound:            false,
		http.StatusConflict:            false,
		http.StatusUnprocessableEntity: false,
		http.StatusNotImplemented:      false,
	}
	for status, want := range cases {
		if got := Retryable(status); got != want {
			t.Errorf("Retryable(%d) = %v, want %v", status, got, want)
		}
	}
}

func TestPolicyDelayDoublesAndCaps(t *testing.T) {
	cases := []struct {
		name       string
		policy     Policy
		attempt    int
		serverSays time.Duration
		want       time.Duration
	}{
		{name: "first retry uses the base delay", policy: Policy{BaseDelay: 100 * time.Millisecond, MaxDelay: time.Second}, attempt: 1, want: 100 * time.Millisecond},
		{name: "second retry doubles", policy: Policy{BaseDelay: 100 * time.Millisecond, MaxDelay: time.Second}, attempt: 2, want: 200 * time.Millisecond},
		{name: "third retry doubles again", policy: Policy{BaseDelay: 100 * time.Millisecond, MaxDelay: time.Second}, attempt: 3, want: 400 * time.Millisecond},
		{name: "the cap wins", policy: Policy{BaseDelay: 100 * time.Millisecond, MaxDelay: 150 * time.Millisecond}, attempt: 3, want: 150 * time.Millisecond},
		{name: "no cap means no cap", policy: Policy{BaseDelay: 100 * time.Millisecond}, attempt: 8, want: 12800 * time.Millisecond},
		{name: "the server's advice replaces the backoff", policy: Policy{BaseDelay: 100 * time.Millisecond, MaxDelay: 5 * time.Second}, attempt: 1, serverSays: 3 * time.Second, want: 3 * time.Second},
		{name: "the server's advice is capped", policy: Policy{BaseDelay: 100 * time.Millisecond, MaxDelay: 5 * time.Second}, attempt: 1, serverSays: 9 * time.Second, want: 5 * time.Second},
		{name: "the server's advice is uncapped with no cap", policy: Policy{BaseDelay: 100 * time.Millisecond}, attempt: 1, serverSays: 9 * time.Second, want: 9 * time.Second},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := testCase.policy.delay(testCase.attempt, testCase.serverSays); got != testCase.want {
				t.Errorf("delay(%d, %v) = %v, want %v", testCase.attempt, testCase.serverSays, got, testCase.want)
			}
		})
	}
}

func TestSleepReturnsImmediatelyForNonPositiveDurations(t *testing.T) {
	started := time.Now()
	if err := Sleep(context.Background(), 0); err != nil {
		t.Errorf("Sleep(0) = %v, want nil", err)
	}
	if err := Sleep(context.Background(), -time.Second); err != nil {
		t.Errorf("Sleep(-1s) = %v, want nil", err)
	}
	if elapsed := time.Since(started); elapsed > 50*time.Millisecond {
		t.Errorf("Sleep returned after %v, want it to return at once", elapsed)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Sleep(cancelled, 0); !errors.Is(err, context.Canceled) {
		t.Errorf("Sleep on a cancelled context = %v, want context.Canceled", err)
	}
	if err := Sleep(cancelled, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("Sleep on a cancelled context = %v, want context.Canceled", err)
	}
}

func TestSleepWaitsForTheDuration(t *testing.T) {
	started := time.Now()
	if err := Sleep(context.Background(), 5*time.Millisecond); err != nil {
		t.Errorf("Sleep = %v, want nil", err)
	}
	if elapsed := time.Since(started); elapsed < 5*time.Millisecond {
		t.Errorf("Sleep returned after %v, want at least 5ms", elapsed)
	}
}

func TestDefaultPolicyRetriesThreeTimesWithASleepSeam(t *testing.T) {
	policy := DefaultPolicy()
	if policy.Attempts != 3 {
		t.Errorf("Attempts = %d, want 3", policy.Attempts)
	}
	if policy.BaseDelay != 200*time.Millisecond {
		t.Errorf("BaseDelay = %v, want 200ms", policy.BaseDelay)
	}
	if policy.MaxDelay != 5*time.Second {
		t.Errorf("MaxDelay = %v, want 5s", policy.MaxDelay)
	}
	if policy.Sleep == nil {
		t.Error("Sleep = nil, want the real Sleep so tests can replace it")
	}
}
