package runner

import (
	"context"
	"errors"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"
)

// waiter lets a test hold a review open until it is done asserting on it.
type waiter struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newWaiter() *waiter {
	return &waiter{started: make(chan struct{}), release: make(chan struct{})}
}

func (w *waiter) wait(ctx context.Context) error {
	w.once.Do(func() { close(w.started) })
	select {
	case <-w.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *waiter) proceed() { close(w.release) }

func quietQueue(options Options) *Queue {
	if options.Logger == nil {
		options.Logger = log.New(io.Discard, "", 0)
	}
	return New(options)
}

func TestSubmitRunsTheReviewAndRecordsTheOutcome(t *testing.T) {
	queue := quietQueue(Options{
		Reviewer: func(context.Context, Job) (Outcome, error) {
			return Outcome{Findings: 3, Posted: true, Comment: 77}, nil
		},
	})

	record := queue.Submit(context.Background(), Job{Repository: "acme/widget", Number: 4, Event: "opened", HeadSHA: "abc123"})
	queue.Wait()

	final, ok := queue.Get(record.ID)
	if !ok {
		t.Fatal("the record disappeared")
	}
	if final.State != StateSucceeded {
		t.Errorf("State = %q, want %q", final.State, StateSucceeded)
	}
	if final.Findings != 3 || !final.Posted || final.CommentID != 77 {
		t.Errorf("the outcome was not recorded: %+v", final)
	}
	if final.HeadSHA != "abc123" {
		t.Errorf("HeadSHA = %q, want abc123", final.HeadSHA)
	}
	if final.StartedAt.IsZero() || final.FinishedAt.IsZero() {
		t.Errorf("the timestamps were not recorded: %+v", final)
	}
	if queue.Active() != 0 {
		t.Errorf("Active() = %d after the run finished, want 0", queue.Active())
	}
}

func TestSubmitOnTheSamePullRequestCancelsTheEarlierRun(t *testing.T) {
	first := newWaiter()
	second := newWaiter()
	var runs int
	var mu sync.Mutex

	queue := quietQueue(Options{
		Reviewer: func(ctx context.Context, job Job) (Outcome, error) {
			mu.Lock()
			runs++
			attempt := runs
			mu.Unlock()

			wait := first
			if attempt == 2 {
				wait = second
			}
			if err := wait.wait(ctx); err != nil {
				return Outcome{}, err
			}
			return Outcome{Findings: attempt, Posted: true}, nil
		},
	})

	firstRecord := queue.Submit(context.Background(), Job{Repository: "acme/widget", Number: 1, Event: "opened"})
	<-first.started

	secondRecord := queue.Submit(context.Background(), Job{Repository: "acme/widget", Number: 1, Event: "synchronize"})

	// The first review learns it was superseded, and must not report an error:
	// being replaced is the intended outcome, not a failure.
	first.proceed()
	second.proceed()
	queue.Wait()

	finished, ok := queue.Get(firstRecord.ID)
	if !ok {
		t.Fatal("the first record disappeared")
	}
	if finished.State != StateCancelled {
		t.Errorf("the superseded run is %q, want %q", finished.State, StateCancelled)
	}
	if finished.Reason != "superseded by a newer push" && finished.Reason != "superseded before it started" {
		t.Errorf("the superseded run's reason is %q, want it to say it was superseded", finished.Reason)
	}
	if finished.Error != "" {
		t.Errorf("being superseded was recorded as an error: %q", finished.Error)
	}

	newer, ok := queue.Get(secondRecord.ID)
	if !ok {
		t.Fatal("the second record disappeared")
	}
	if newer.State != StateSucceeded {
		t.Errorf("the newer run is %q, want %q", newer.State, StateSucceeded)
	}
	if newer.Findings != 2 {
		t.Errorf("the newer run reviewed the wrong thing: %+v", newer)
	}
}

func TestSubmitDoesNotCancelARunForAnotherPullRequest(t *testing.T) {
	blocker := newWaiter()
	queue := quietQueue(Options{
		MaxConcurrent: 2,
		Reviewer: func(ctx context.Context, job Job) (Outcome, error) {
			if job.Number == 1 {
				if err := blocker.wait(ctx); err != nil {
					return Outcome{}, err
				}
			}
			return Outcome{Posted: true}, nil
		},
	})

	first := queue.Submit(context.Background(), Job{Repository: "acme/widget", Number: 1})
	<-blocker.started
	other := queue.Submit(context.Background(), Job{Repository: "acme/widget", Number: 2})

	blocker.proceed()
	queue.Wait()

	if record, _ := queue.Get(first.ID); record.State != StateSucceeded {
		t.Errorf("the unrelated run was disturbed: %+v", record)
	}
	if record, _ := queue.Get(other.ID); record.State != StateSucceeded {
		t.Errorf("the second pull request did not finish: %+v", record)
	}
}

func TestFailedRunIsRecordedAndReported(t *testing.T) {
	var reported []Record
	queue := quietQueue(Options{
		Reviewer: func(context.Context, Job) (Outcome, error) {
			return Outcome{Findings: 1}, errors.New("could not post the comment: 403 Forbidden")
		},
		OnFailure: func(record Record) { reported = append(reported, record) },
	})

	record := queue.Submit(context.Background(), Job{Repository: "acme/widget", Number: 9})
	queue.Wait()

	finished, _ := queue.Get(record.ID)
	if finished.State != StateFailed {
		t.Errorf("State = %q, want %q", finished.State, StateFailed)
	}
	if !strings.Contains(finished.Error, "403 Forbidden") {
		t.Errorf("the failure was not recorded: %+v", finished)
	}
	if len(reported) != 1 {
		t.Fatalf("the failure was reported %d time(s), want once", len(reported))
	}
	// The notification has to carry the id, or nobody can find the run it is
	// talking about.
	if reported[0].ID != record.ID {
		t.Errorf("the report names run %q, want %q", reported[0].ID, record.ID)
	}
}

func TestSkippedRunIsNotAFailure(t *testing.T) {
	var reported int
	queue := quietQueue(Options{
		Reviewer: func(context.Context, Job) (Outcome, error) {
			return Outcome{Skipped: true, Reason: "the review found nothing to report"}, nil
		},
		OnFailure: func(Record) { reported++ },
	})

	record := queue.Submit(context.Background(), Job{Repository: "acme/widget", Number: 3})
	queue.Wait()

	finished, _ := queue.Get(record.ID)
	if finished.State != StateSkipped {
		t.Errorf("State = %q, want %q", finished.State, StateSkipped)
	}
	if finished.Reason != "the review found nothing to report" {
		t.Errorf("Reason = %q, want the outcome's reason", finished.Reason)
	}
	if reported != 0 {
		t.Error("a quiet review was reported as a failure, which would train people to ignore the alerts")
	}
}

func TestConcurrencyIsBounded(t *testing.T) {
	var mu sync.Mutex
	inFlight, peak := 0, 0

	queue := quietQueue(Options{
		MaxConcurrent: 2,
		Reviewer: func(context.Context, Job) (Outcome, error) {
			mu.Lock()
			inFlight++
			if inFlight > peak {
				peak = inFlight
			}
			mu.Unlock()

			time.Sleep(15 * time.Millisecond)

			mu.Lock()
			inFlight--
			mu.Unlock()
			return Outcome{Posted: true}, nil
		},
	})

	// Different pull requests, so none of them supersedes another.
	for number := 1; number <= 6; number++ {
		queue.Submit(context.Background(), Job{Repository: "acme/widget", Number: number})
	}
	queue.Wait()

	if peak > 2 {
		t.Errorf("%d reviews ran at once, want at most 2", peak)
	}
	if peak < 2 {
		t.Errorf("only %d review ran at a time: the slots are not being used", peak)
	}
}

func TestListIsNewestFirst(t *testing.T) {
	queue := quietQueue(Options{
		Reviewer: func(context.Context, Job) (Outcome, error) { return Outcome{Posted: true}, nil },
	})

	var ids []string
	for number := 1; number <= 3; number++ {
		ids = append(ids, queue.Submit(context.Background(), Job{Repository: "acme/widget", Number: number}).ID)
		time.Sleep(2 * time.Millisecond)
	}
	queue.Wait()

	records := queue.List()
	if len(records) != 3 {
		t.Fatalf("List returned %d records, want 3", len(records))
	}
	if records[0].ID != ids[2] || records[2].ID != ids[0] {
		t.Errorf("List returned %s, %s, %s; want the newest first", records[0].ID, records[1].ID, records[2].ID)
	}
}

func TestGetUnknownRun(t *testing.T) {
	queue := quietQueue(Options{Reviewer: func(context.Context, Job) (Outcome, error) { return Outcome{}, nil }})
	if _, ok := queue.Get("nope"); ok {
		t.Error("Get returned a record for an id that was never submitted")
	}
}

// TestRunSurvivesTheCancellationOfTheRequestThatQueuedIt covers the bug that
// only a real webhook delivery shows: an HTTP handler's context is cancelled
// the moment its response is written, and a run tied to it would be cancelled
// by the act of acknowledging the delivery.
func TestRunSurvivesTheCancellationOfTheRequestThatQueuedIt(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	queue := quietQueue(Options{
		Reviewer: func(ctx context.Context, job Job) (Outcome, error) {
			close(started)
			select {
			case <-release:
				return Outcome{Findings: 2, Posted: true}, nil
			case <-ctx.Done():
				return Outcome{}, ctx.Err()
			}
		},
	})

	request, cancel := context.WithCancel(context.Background())
	record := queue.Submit(request, Job{Repository: "acme/widget", Number: 1})
	<-started

	// The response has been written, so net/http cancels the request context.
	cancel()
	close(release)
	queue.Wait()

	finished, _ := queue.Get(record.ID)
	if finished.State != StateSucceeded {
		t.Fatalf("State = %q after the delivery was acknowledged, want %q: the run must outlive the request",
			finished.State, StateSucceeded)
	}
	if finished.Findings != 2 {
		t.Errorf("Findings = %d, want 2", finished.Findings)
	}
}

func TestSubmitUsesTheSuppliedClockAndIDs(t *testing.T) {
	moment := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	queue := quietQueue(Options{
		Reviewer: func(context.Context, Job) (Outcome, error) { return Outcome{Posted: true}, nil },
		Now:      func() time.Time { return moment },
		NewID:    func() string { return "run-1" },
	})

	record := queue.Submit(context.Background(), Job{Repository: "acme/widget", Number: 1})
	queue.Wait()

	if record.ID != "run-1" {
		t.Errorf("ID = %q, want run-1", record.ID)
	}
	if !record.QueuedAt.Equal(moment) {
		t.Errorf("QueuedAt = %v, want %v", record.QueuedAt, moment)
	}
	finished, _ := queue.Get("run-1")
	if !finished.FinishedAt.Equal(moment) {
		t.Errorf("FinishedAt = %v, want %v", finished.FinishedAt, moment)
	}
}

func TestCancelBeforeASlotIsFreeDoesNotRunTheReview(t *testing.T) {
	blocker := newWaiter()
	var mu sync.Mutex
	reviewed := map[string]int{}

	queue := quietQueue(Options{
		MaxConcurrent: 1,
		Reviewer: func(ctx context.Context, job Job) (Outcome, error) {
			mu.Lock()
			reviewed[job.HeadSHA]++
			mu.Unlock()
			if job.Number == 1 {
				if err := blocker.wait(ctx); err != nil {
					return Outcome{}, err
				}
			}
			return Outcome{Posted: true}, nil
		},
	})

	// The first pull request holds the only slot. The second is queued behind
	// it, and a new push to that same pull request replaces it before it ever
	// starts: the model must never be asked about the older revision.
	queue.Submit(context.Background(), Job{Repository: "acme/widget", Number: 1, HeadSHA: "one"})
	<-blocker.started

	superseded := queue.Submit(context.Background(), Job{Repository: "acme/widget", Number: 2, HeadSHA: "old-sha"})
	newer := queue.Submit(context.Background(), Job{Repository: "acme/widget", Number: 2, HeadSHA: "new-sha"})

	blocker.proceed()
	queue.Wait()

	mu.Lock()
	defer mu.Unlock()
	if reviewed["old-sha"] != 0 {
		t.Errorf("the superseded revision was reviewed anyway: %v", reviewed)
	}
	if reviewed["new-sha"] != 1 {
		t.Errorf("the newest revision was reviewed %d time(s), want once: %v", reviewed["new-sha"], reviewed)
	}
	if record, _ := queue.Get(superseded.ID); record.State != StateCancelled {
		t.Errorf("the superseded run is %q, want %q", record.State, StateCancelled)
	}
	if record, _ := queue.Get(newer.ID); record.State != StateSucceeded {
		t.Errorf("the newest run is %q, want %q", record.State, StateSucceeded)
	}
}
