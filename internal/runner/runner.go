// Package runner turns review requests into runs: it decides what may run at
// once, cancels the work that a newer push has made pointless, and keeps a
// record of every run so that a review that never appeared can be accounted
// for.
//
// This is the answer to the two failure modes the challenge opens with: a
// review that silently disappears, and three contradictory comments from three
// pushes that arrived together.
package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"sort"
	"sync"
	"time"
)

// Run states.
const (
	StateQueued    = "queued"
	StateRunning   = "running"
	StateSucceeded = "succeeded"
	StateFailed    = "failed"
	StateCancelled = "cancelled"
	StateSkipped   = "skipped"
)

// Record is what happened to one run. The id is what a log line, a
// notification and the runs endpoint all refer to.
type Record struct {
	ID         string    `json:"id"`
	Repository string    `json:"repository"`
	Number     int       `json:"number"`
	Event      string    `json:"event"`
	HeadSHA    string    `json:"head_sha"`
	State      string    `json:"state"`
	Reason     string    `json:"reason,omitempty"`
	Findings   int       `json:"findings"`
	Posted     bool      `json:"posted"`
	CommentID  int64     `json:"comment_id,omitempty"`
	Error      string    `json:"error,omitempty"`
	QueuedAt   time.Time `json:"queued_at"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
}

// Duration is how long the run took, once it has finished.
func (r Record) Duration() time.Duration {
	if r.StartedAt.IsZero() || r.FinishedAt.IsZero() {
		return 0
	}
	return r.FinishedAt.Sub(r.StartedAt)
}

// Job is a request to review a pull request.
type Job struct {
	Repository string
	Number     int
	Event      string
	HeadSHA    string
}

// Outcome is what a review produced, without the runner needing to know what a
// review is.
type Outcome struct {
	Findings int
	Posted   bool
	Comment  int64
	Skipped  bool
	Reason   string
}

// Reviewer performs one review. It must respect the context: a newer push
// cancels it, and a cancelled review must not post.
type Reviewer func(ctx context.Context, job Job) (Outcome, error)

// Options configure a queue.
type Options struct {
	Reviewer      Reviewer
	MaxConcurrent int
	Logger        *log.Logger
	Now           func() time.Time
	NewID         func() string

	// OnFailure is called with the finished record when a run fails. It is how
	// a failed review reaches a human instead of looking like a pull request
	// nobody reviewed.
	OnFailure func(Record)
}

// Queue bounds the work and keeps the records.
type Queue struct {
	mu        sync.Mutex
	active    map[string]*activeRun
	records   map[string]*Record
	order     []string
	reviewer  Reviewer
	logger    *log.Logger
	now       func() time.Time
	newID     func() string
	onFailure func(Record)
	slots     chan struct{}
	wg        sync.WaitGroup
}

type activeRun struct {
	id     string
	cancel context.CancelFunc
}

// New builds a queue.
func New(options Options) *Queue {
	logger := options.Logger
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	concurrency := options.MaxConcurrent
	if concurrency < 1 {
		concurrency = 1
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	newID := options.NewID
	if newID == nil {
		newID = randomID
	}
	return &Queue{
		active:    make(map[string]*activeRun),
		records:   make(map[string]*Record),
		reviewer:  options.Reviewer,
		logger:    logger,
		now:       now,
		newID:     newID,
		onFailure: options.OnFailure,
		slots:     make(chan struct{}, concurrency),
	}
}

// Submit queues a review.
//
// A run for the same pull request that is still going is cancelled first,
// because a review of the final state is worth more than two reviews of states
// nobody will look at again.
//
// The run does not inherit the caller's cancellation. A webhook delivery's
// context is cancelled the instant the 202 is written, so a run tied to it
// would be cancelled by the very act of acknowledging the delivery: every
// review would stop before it read anything. A run ends when it finishes, when
// a newer push supersedes it, or when the process exits.
func (q *Queue) Submit(ctx context.Context, job Job) Record {
	q.mu.Lock()
	key := fmt.Sprintf("%s#%d", job.Repository, job.Number)
	if running, ok := q.active[key]; ok {
		q.logger.Printf("%s#%d: cancelling run %s, superseded", job.Repository, job.Number, running.id)
		if record, ok := q.records[running.id]; ok {
			record.Reason = "superseded by a newer push"
		}
		running.cancel()
	}

	id := q.newID()
	record := &Record{
		ID:         id,
		Repository: job.Repository,
		Number:     job.Number,
		Event:      job.Event,
		HeadSHA:    job.SHA(),
		State:      StateQueued,
		QueuedAt:   q.now(),
	}
	q.records[id] = record
	q.order = append(q.order, id)

	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	q.active[key] = &activeRun{id: id, cancel: cancel}
	q.wg.Add(1)
	go q.run(runCtx, job, id, key)

	// The copy is taken under the lock: the run is already going, and it may be
	// writing to this record already.
	created := record.snapshot()
	q.mu.Unlock()

	return created
}

// Get returns one record.
func (q *Queue) Get(id string) (Record, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	record, ok := q.records[id]
	if !ok {
		return Record{}, false
	}
	return record.snapshot(), true
}

// List returns every record, newest first.
func (q *Queue) List() []Record {
	q.mu.Lock()
	defer q.mu.Unlock()
	records := make([]Record, 0, len(q.order))
	for _, id := range q.order {
		if record, ok := q.records[id]; ok {
			records = append(records, record.snapshot())
		}
	}
	sort.SliceStable(records, func(i, j int) bool { return records[i].QueuedAt.After(records[j].QueuedAt) })
	return records
}

// Active counts the runs that have not finished.
func (q *Queue) Active() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.active)
}

// Wait blocks until every queued run has finished. It is for tests and for a
// graceful shutdown.
func (q *Queue) Wait() { q.wg.Wait() }

func (q *Queue) run(ctx context.Context, job Job, id, key string) {
	defer q.wg.Done()

	// Wait for a slot, or give up if a newer push has already superseded this
	// run while it was queued.
	select {
	case q.slots <- struct{}{}:
		defer func() { <-q.slots }()
	case <-ctx.Done():
		q.finish(id, key, StateCancelled, "superseded before it started", Outcome{}, ctx.Err())
		return
	}

	// The slot can arrive in the same instant as the cancellation, and select
	// picks between two ready cases at random. Checking again here means a
	// superseded run never enters the reviewer at all, rather than entering it
	// to fail immediately.
	if ctx.Err() != nil {
		q.finish(id, key, StateCancelled, "superseded before it started", Outcome{}, ctx.Err())
		return
	}

	q.update(id, func(record *Record) {
		record.State = StateRunning
		record.StartedAt = q.now()
	})

	outcome, err := q.reviewer(ctx, job)

	switch {
	case ctx.Err() != nil:
		q.finish(id, key, StateCancelled, "superseded by a newer push", outcome, nil)
	case err != nil:
		q.finish(id, key, StateFailed, "", outcome, err)
	case outcome.Skipped:
		q.finish(id, key, StateSkipped, outcome.Reason, outcome, nil)
	default:
		q.finish(id, key, StateSucceeded, outcome.Reason, outcome, nil)
	}
}

func (q *Queue) finish(id, key, state, reason string, outcome Outcome, err error) {
	snapshot := func() (Record, bool) {
		q.mu.Lock()
		defer q.mu.Unlock()

		record, ok := q.records[id]
		if !ok {
			return Record{}, false
		}
		record.State = state
		record.Findings = outcome.Findings
		record.Posted = outcome.Posted
		record.CommentID = outcome.Comment
		if record.Reason == "" {
			record.Reason = reason
		}
		if err != nil {
			record.Error = err.Error()
		}
		if !record.StartedAt.IsZero() || state != StateCancelled {
			record.FinishedAt = q.now()
		}
		// Only the newest run for a pull request stays in the active map, so a
		// cancelled run must not evict the run that replaced it.
		if active, ok := q.active[key]; ok && active.id == id {
			delete(q.active, key)
		}
		copied := *record
		return copied, true
	}

	finished, ok := snapshot()
	if !ok {
		return
	}
	q.logger.Printf("run %s for %s#%d finished: %s", finished.ID, finished.Repository, finished.Number, finished.State)
	if err != nil && q.onFailure != nil {
		// The notification must not be able to take the run down with it.
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					q.logger.Printf("the failure notifier panicked: %v", recovered)
				}
			}()
			q.onFailure(finished)
		}()
	}
}

func (q *Queue) update(id string, apply func(*Record)) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if record, ok := q.records[id]; ok {
		apply(record)
	}
}

// snapshot copies a record so that a caller cannot hold the lock or mutate it.
func (r *Record) snapshot() Record {
	copied := *r
	return copied
}

// SHA is the revision the job is for.
func (j Job) SHA() string { return j.HeadSHA }

func randomID() string {
	buffer := make([]byte, 4)
	if _, err := rand.Read(buffer); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buffer)
}
