package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wiztools/knowledged/internal/organizer"
	"github.com/wiztools/knowledged/internal/store"
)

// Status values for a Job.
type Status string

const (
	StatusQueued     Status = "queued"
	StatusProcessing Status = "processing"
	StatusDone       Status = "done"
	StatusFailed     Status = "failed"
)

// Job represents one unit of work: store a piece of content.
type Job struct {
	ID        string    `json:"id"`
	Status    Status    `json:"status"`
	Timestamp time.Time `json:"ts"`
	Content   string    `json:"content"`
	Hint      string    `json:"hint,omitempty"`
	Tags      []string  `json:"tags,omitempty"`
	Path      string    `json:"path,omitempty"`  // set after successful store
	Error     string    `json:"error,omitempty"` // set on failure
}

// Queue is a file-backed, single-worker job queue.
//
// Durability contract:
//   - Every job is appended to queue.json (unversioned) before the HTTP
//     handler returns, so no job is silently lost on crash.
//   - The single worker goroutine marks a job "processing" before it starts
//     work and "done" / "failed" after it finishes.
//   - On startup, any job stuck in "processing" is reconciled against the
//     git log: if the commit already exists the job is marked done; otherwise
//     it is reset to "queued" and retried.
type Queue struct {
	path      string // absolute path to queue.json
	mu        sync.Mutex
	signal    chan struct{}
	results   map[string]*Job // in-memory copy of completed jobs for fast GET /jobs lookups
	resultsMu sync.RWMutex

	store     *store.Store
	organizer *organizer.Organizer
	logger    *slog.Logger
}

// New creates a Queue, runs startup reconciliation, and returns.
func New(st *store.Store, org *organizer.Organizer, logger *slog.Logger) (*Queue, error) {
	q := &Queue{
		path:      filepath.Join(st.RepoPath(), "queue.json"),
		signal:    make(chan struct{}, 256),
		results:   make(map[string]*Job),
		store:     st,
		organizer: org,
		logger:    logger,
	}
	if err := q.reconcile(); err != nil {
		return nil, fmt.Errorf("queue reconciliation failed: %w", err)
	}
	return q, nil
}

// Enqueue appends a new job to the persistent queue and signals the worker.
func (q *Queue) Enqueue(content, hint string, tags []string) (*Job, error) {
	job := &Job{
		ID:        uuid.New().String(),
		Status:    StatusQueued,
		Timestamp: time.Now().UTC(),
		Content:   content,
		Hint:      hint,
		Tags:      tags,
	}

	q.mu.Lock()
	jobs, err := q.loadJobs()
	if err != nil {
		q.mu.Unlock()
		return nil, fmt.Errorf("loading queue: %w", err)
	}
	jobs = append(jobs, job)
	if err := q.saveJobs(jobs); err != nil {
		q.mu.Unlock()
		return nil, fmt.Errorf("saving queue: %w", err)
	}
	q.mu.Unlock()

	q.logger.Info("job enqueued", "job_id", job.ID)

	// Non-blocking signal — the channel is buffered.
	select {
	case q.signal <- struct{}{}:
	default:
	}

	return job, nil
}

// GetJob returns a job by ID. It checks the in-memory results map first, then
// falls back to the persistent queue file so callers can query any job state.
func (q *Queue) GetJob(id string) (*Job, bool) {
	q.resultsMu.RLock()
	if j, ok := q.results[id]; ok {
		q.resultsMu.RUnlock()
		return j, true
	}
	q.resultsMu.RUnlock()

	// Fall back to queue file for jobs still pending/processing.
	q.mu.Lock()
	defer q.mu.Unlock()
	jobs, err := q.loadJobs()
	if err != nil {
		return nil, false
	}
	for _, j := range jobs {
		if j.ID == id {
			return j, true
		}
	}
	return nil, false
}

// Start launches the single worker goroutine. It blocks until ctx is cancelled.
func (q *Queue) Start(ctx context.Context) {
	q.logger.Info("queue worker started")
	for {
		select {
		case <-ctx.Done():
			q.logger.Info("queue worker stopped")
			return
		case <-q.signal:
			q.drainQueue(ctx)
		}
	}
}

// drainQueue processes all currently queued jobs one at a time.
func (q *Queue) drainQueue(ctx context.Context) {
	for {
		job := q.nextQueued()
		if job == nil {
			return
		}
		q.processJob(ctx, job)
	}
}

// nextQueued returns the oldest job with status "queued", atomically marking
// it "processing". Returns nil when the queue is empty.
func (q *Queue) nextQueued() *Job {
	q.mu.Lock()
	defer q.mu.Unlock()

	jobs, err := q.loadJobs()
	if err != nil {
		q.logger.Error("failed to load queue", "error", err)
		return nil
	}
	for _, j := range jobs {
		if j.Status == StatusQueued {
			j.Status = StatusProcessing
			if err := q.saveJobs(jobs); err != nil {
				q.logger.Error("failed to mark job as processing", "job_id", j.ID, "error", err)
				return nil
			}
			q.logger.Info("picked up job for processing", "job_id", j.ID)
			return j
		}
	}
	return nil
}

// processJob runs the organizer for a single job and updates its final status.
func (q *Queue) processJob(ctx context.Context, job *Job) {
	q.logger.Info("processing job", "job_id", job.ID)

	decision, err := q.organizer.Decide(ctx, job.Content, job.Hint, job.Tags)
	if err != nil {
		q.logger.Error("organizer decision failed", "job_id", job.ID, "error", err)
		q.finalise(job, "", err)
		return
	}

	if err := q.organizer.Execute(ctx, job.ID, job.Content, decision); err != nil {
		q.logger.Error("organizer execute failed", "job_id", job.ID, "error", err)
		q.finalise(job, "", err)
		return
	}

	q.logger.Info("job completed successfully", "job_id", job.ID, "path", decision.TargetPath)
	q.finalise(job, decision.TargetPath, nil)
}

// finalise updates the job's terminal status in the queue file and results map.
func (q *Queue) finalise(job *Job, path string, execErr error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	jobs, err := q.loadJobs()
	if err != nil {
		q.logger.Error("failed to load queue during finalise", "job_id", job.ID, "error", err)
		return
	}
	for _, j := range jobs {
		if j.ID == job.ID {
			if execErr != nil {
				j.Status = StatusFailed
				j.Error = execErr.Error()
			} else {
				j.Status = StatusDone
				j.Path = path
			}
			// Update the in-memory pointer so our results copy is also current.
			*job = *j
			break
		}
	}
	if err := q.saveJobs(jobs); err != nil {
		q.logger.Error("failed to save queue during finalise", "job_id", job.ID, "error", err)
	}

	// Cache in results map for fast lookups after the job is gone from the file.
	jobCopy := *job
	q.resultsMu.Lock()
	q.results[job.ID] = &jobCopy
	q.resultsMu.Unlock()
}

// reconcile is called once on startup. It handles any jobs left in a
// non-terminal state from a previous run.
//
//   - done / failed  → loaded into in-memory results map, kept in file
//   - queued         → re-signal worker
//   - processing     → check git log; done if committed, else reset to queued
func (q *Queue) reconcile() error {
	q.mu.Lock()
	defer q.mu.Unlock()

	jobs, err := q.loadJobs()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			q.logger.Info("no queue file found — starting fresh")
			return nil
		}
		return fmt.Errorf("loading queue file: %w", err)
	}

	q.logger.Info("reconciling queue on startup", "total_jobs", len(jobs))

	changed := false
	for _, job := range jobs {
		switch job.Status {
		case StatusDone, StatusFailed:
			q.results[job.ID] = job
			q.logger.Debug("loaded terminal job into results map",
				"job_id", job.ID, "status", job.Status)

		case StatusQueued:
			q.logger.Info("found pending job — will retry", "job_id", job.ID)
			select {
			case q.signal <- struct{}{}:
			default:
			}

		case StatusProcessing:
			q.logger.Warn("found job stuck in processing state — checking git log",
				"job_id", job.ID)
			found, err := q.store.FindCommitByJobID(job.ID)
			if err != nil {
				q.logger.Warn("git log check failed — resetting job to queued",
					"job_id", job.ID, "error", err)
				job.Status = StatusQueued
			} else if found {
				q.logger.Info("git commit found — marking job done (crash recovery)",
					"job_id", job.ID)
				job.Status = StatusDone
				q.results[job.ID] = job
			} else {
				q.logger.Info("no git commit found — resetting job to queued for retry",
					"job_id", job.ID)
				job.Status = StatusQueued
				select {
				case q.signal <- struct{}{}:
				default:
				}
			}
			changed = true
		}
	}

	if changed {
		if err := q.saveJobs(jobs); err != nil {
			return fmt.Errorf("saving reconciled queue: %w", err)
		}
	}
	return nil
}

// loadJobs reads queue.json. Returns os.ErrNotExist if the file does not exist.
func (q *Queue) loadJobs() ([]*Job, error) {
	data, err := os.ReadFile(q.path)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	var jobs []*Job
	if err := json.Unmarshal(data, &jobs); err != nil {
		return nil, fmt.Errorf("parsing queue.json: %w", err)
	}
	return jobs, nil
}

// saveJobs atomically rewrites queue.json.
func (q *Queue) saveJobs(jobs []*Job) error {
	data, err := json.MarshalIndent(jobs, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling queue: %w", err)
	}
	// Write to a temp file then rename for atomicity.
	tmp := q.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("writing temp queue file: %w", err)
	}
	if err := os.Rename(tmp, q.path); err != nil {
		return fmt.Errorf("renaming queue file: %w", err)
	}
	return nil
}
