package queue

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wiztools/knowledged/internal/organizer"
	"github.com/wiztools/knowledged/internal/recentlog"
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

// resultsTTL is how long a completed job stays queryable via GET /jobs/{id}.
const resultsTTL = time.Hour

const originPushStateFile = "origin-push.json"

// Job represents one unit of work: store or delete a piece of content.
type Job struct {
	ID          string     `json:"id"`
	Status      Status     `json:"status"`
	Timestamp   time.Time  `json:"ts"`
	CompletedAt *time.Time `json:"completed_at,omitempty"` // set when terminal; used for TTL eviction
	Operation   string     `json:"operation,omitempty"`    // "post" or "delete"; empty means "post" (backward compat)
	Content     string     `json:"content,omitempty"`
	ContentHash string     `json:"content_hash,omitempty"` // SHA-256 of Content; used for deduplication
	Hint        string     `json:"hint,omitempty"`
	Tags        []string   `json:"tags,omitempty"`
	Path        string     `json:"path,omitempty"`  // target path; set on enqueue for delete, after store for post
	Error       string     `json:"error,omitempty"` // set on failure
}

// Queue is a file-backed, single-worker job queue.
//
// Durability contract:
//   - Every job is appended to .knowledged/queue.json (unversioned) before the HTTP
//     handler returns, so no job is silently lost on crash.
//   - The single worker goroutine marks a job "processing" before it starts
//     work and "done" / "failed" after it finishes.
//   - On startup, any job stuck in "processing" is reconciled against the
//     git log: if the commit already exists the job is marked done; otherwise
//     it is reset to "queued" and retried.
type Queue struct {
	path      string // absolute path to .knowledged/queue.json
	mu        sync.Mutex
	signal    chan struct{}
	results   map[string]*Job // in-memory copy of completed jobs for fast GET /jobs lookups
	resultsMu sync.RWMutex

	store           *store.Store
	organizer       *organizer.Organizer
	recentLog       *recentlog.RecentLog
	logger          *slog.Logger
	pushOriginEvery time.Duration
	pushStatePath   string
	newTimer        func(time.Duration) queueTicker
}

type queueTicker interface {
	C() <-chan time.Time
	Stop() bool
	Reset(time.Duration) bool
}

type timeTimer struct {
	*time.Timer
}

func (t *timeTimer) C() <-chan time.Time { return t.Timer.C }

type originPushState struct {
	LastAttemptAt time.Time `json:"last_attempt_at"`
}

// New creates a Queue, runs startup reconciliation, and returns.
func New(st *store.Store, org *organizer.Organizer, rl *recentlog.RecentLog, logger *slog.Logger, pushOriginEvery time.Duration) (*Queue, error) {
	q := &Queue{
		path:            st.StatePath("queue.json"),
		signal:          make(chan struct{}, 256),
		results:         make(map[string]*Job),
		store:           st,
		organizer:       org,
		recentLog:       rl,
		logger:          logger,
		pushOriginEvery: pushOriginEvery,
		pushStatePath:   st.StatePath(originPushStateFile),
		newTimer: func(d time.Duration) queueTicker {
			return &timeTimer{Timer: time.NewTimer(d)}
		},
	}
	if err := q.reconcile(); err != nil {
		return nil, fmt.Errorf("queue reconciliation failed: %w", err)
	}
	return q, nil
}

// contentHash returns the hex-encoded SHA-256 hash of the given content string.
func contentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// Enqueue appends a new job to the persistent queue and signals the worker.
// If a job with the same content is already queued or processing, the existing
// job is returned without creating a duplicate.
func (q *Queue) Enqueue(content, hint string, tags []string) (*Job, error) {
	hash := contentHash(content)

	q.mu.Lock()
	jobs, err := q.loadJobs()
	if err != nil {
		q.mu.Unlock()
		return nil, fmt.Errorf("loading queue: %w", err)
	}

	for _, j := range jobs {
		if j.ContentHash == hash && (j.Status == StatusQueued || j.Status == StatusProcessing) {
			q.mu.Unlock()
			q.logger.Info("duplicate content detected — returning existing job",
				"existing_job_id", j.ID, "content_hash", hash)
			return j, nil
		}
	}

	job := &Job{
		ID:          uuid.New().String(),
		Status:      StatusQueued,
		Timestamp:   time.Now().UTC(),
		Content:     content,
		ContentHash: hash,
		Hint:        hint,
		Tags:        tags,
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

// EnqueueDelete appends a new delete job to the persistent queue and signals
// the worker. The file at path will be removed along with its INDEX.md entry.
func (q *Queue) EnqueueDelete(path string) (*Job, error) {
	job := &Job{
		ID:        uuid.New().String(),
		Status:    StatusQueued,
		Timestamp: time.Now().UTC(),
		Operation: "delete",
		Path:      path,
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

	q.logger.Info("delete job enqueued", "job_id", job.ID, "path", path)

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

// Start launches the worker and the TTL eviction goroutine.
// It blocks until ctx is canceled.
func (q *Queue) Start(ctx context.Context) {
	q.logger.Info("queue worker started")
	go q.evictExpiredResults(ctx)

	var pushTimer queueTicker
	if q.pushOriginEvery > 0 {
		delay, err := q.nextPushDelay(time.Now().UTC())
		if err != nil {
			q.logger.Error("failed to load persisted origin push schedule", "error", err)
			delay = 0
		}
		pushTimer = q.newTimer(delay)
		defer pushTimer.Stop()
		q.logger.Info("periodic git push enabled", "interval", q.pushOriginEvery)
	}

	var pushCh <-chan time.Time
	if pushTimer != nil {
		pushCh = pushTimer.C()
	}

	for {
		select {
		case <-ctx.Done():
			q.logger.Info("queue worker stopped")
			return
		case <-pushCh:
			q.runScheduledPush()
			pushTimer.Reset(q.pushOriginEvery)
		case <-q.signal:
			q.drainQueue(ctx)
		}
	}
}

// evictExpiredResults periodically removes completed jobs from the in-memory
// results map once they exceed resultsTTL. Runs every 5 minutes.
func (q *Queue) evictExpiredResults(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			q.resultsMu.Lock()
			evicted := 0
			for id, job := range q.results {
				if job.CompletedAt != nil && time.Since(*job.CompletedAt) > resultsTTL {
					delete(q.results, id)
					evicted++
				}
			}
			q.resultsMu.Unlock()
			if evicted > 0 {
				q.logger.Info("evicted expired job results from memory",
					"count", evicted, "ttl", resultsTTL)
			}
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

func (q *Queue) nextPushDelay(now time.Time) (time.Duration, error) {
	lastAttempt, ok, err := q.readLastOriginPushAttempt()
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, nil
	}
	nextAt := lastAttempt.Add(q.pushOriginEvery)
	if !nextAt.After(now) {
		return 0, nil
	}
	return nextAt.Sub(now), nil
}

func (q *Queue) readLastOriginPushAttempt() (time.Time, bool, error) {
	data, err := os.ReadFile(q.pushStatePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, fmt.Errorf("reading push state: %w", err)
	}

	var state originPushState
	if err := json.Unmarshal(data, &state); err != nil {
		return time.Time{}, false, fmt.Errorf("parsing push state: %w", err)
	}
	if state.LastAttemptAt.IsZero() {
		return time.Time{}, false, nil
	}
	return state.LastAttemptAt, true, nil
}

func (q *Queue) writeLastOriginPushAttempt(at time.Time) error {
	data, err := json.MarshalIndent(originPushState{LastAttemptAt: at}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling push state: %w", err)
	}
	tmp := q.pushStatePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("writing temp push state: %w", err)
	}
	if err := os.Rename(tmp, q.pushStatePath); err != nil {
		return fmt.Errorf("renaming push state: %w", err)
	}
	return nil
}

func (q *Queue) runScheduledPush() {
	hasOrigin, err := q.store.HasOriginRemote()
	if err != nil {
		q.logger.Error("failed to check origin remote before scheduled push", "error", err)
		return
	}
	if !hasOrigin {
		return
	}

	attemptedAt := time.Now().UTC()
	if err := q.writeLastOriginPushAttempt(attemptedAt); err != nil {
		q.logger.Error("failed to persist origin push state", "error", err)
	}
	q.pushOriginCurrentBranch()
}

func (q *Queue) pushOriginCurrentBranch() {
	if err := q.store.PushOriginCurrentBranch(); err != nil {
		q.logger.Error("periodic git push failed", "error", err)
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

// processJob dispatches to the appropriate handler based on job.Operation.
func (q *Queue) processJob(ctx context.Context, job *Job) {
	q.logger.Info("processing job", "job_id", job.ID, "operation", job.Operation)

	if job.Operation == "delete" {
		q.executeDelete(ctx, job)
		return
	}

	// Default: "post" or empty (backward compat with existing queue entries).
	decision, err := q.organizer.Decide(ctx, job.Content, job.Hint, job.Tags)
	if err != nil {
		q.logger.Error("organizer decision failed", "job_id", job.ID, "error", err)
		q.finalize(job, "", err)
		return
	}

	if err := q.organizer.Execute(ctx, job.ID, job.Content, decision); err != nil {
		q.logger.Error("organizer execute failed", "job_id", job.ID, "error", err)
		q.finalize(job, "", err)
		return
	}

	q.logger.Info("job completed successfully", "job_id", job.ID, "path", decision.TargetPath)
	q.finalize(job, decision.TargetPath, nil)

	if q.recentLog != nil {
		e := recentlog.Entry{
			JobID:     job.ID,
			Path:      decision.TargetPath,
			Tags:      job.Tags,
			CreatedAt: job.Timestamp,
		}
		if err := q.recentLog.Append(e); err != nil {
			q.logger.Warn("recentlog: append failed", "job_id", job.ID, "error", err)
		}
	}
}

// executeDelete removes a file and its INDEX.md entry as a single atomic git commit.
func (q *Queue) executeDelete(ctx context.Context, job *Job) {
	if !q.store.FileExists(job.Path) {
		err := fmt.Errorf("file not found: %s", job.Path)
		q.logger.Error("delete job failed — file does not exist", "job_id", job.ID, "path", job.Path)
		q.finalize(job, "", err)
		return
	}

	if err := q.store.DeleteFile(job.Path); err != nil {
		q.logger.Error("DeleteFile failed", "job_id", job.ID, "path", job.Path, "error", err)
		q.finalize(job, "", err)
		return
	}

	if err := q.store.RemoveIndexEntry(job.Path); err != nil {
		q.logger.Error("RemoveIndexEntry failed", "job_id", job.ID, "path", job.Path, "error", err)
		q.finalize(job, "", err)
		return
	}

	msg := fmt.Sprintf("delete(%s): %s", job.ID, job.Path)
	if err := q.store.Commit(msg); err != nil {
		q.logger.Error("commit failed after delete", "job_id", job.ID, "error", err)
		q.finalize(job, "", err)
		return
	}

	q.logger.Info("delete job completed", "job_id", job.ID, "path", job.Path)
	q.finalize(job, job.Path, nil)
}

// finalize marks a job terminal and updates .knowledged/queue.json:
//   - done  → removed from the file (no retry needed)
//   - failed → kept in the file so the next server startup retries it
//
// Either way the job is cached in the results map for GET /jobs/{id} lookups
// within the TTL window.
func (q *Queue) finalize(job *Job, path string, execErr error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	now := time.Now().UTC()

	jobs, err := q.loadJobs()
	if err != nil {
		q.logger.Error("failed to load queue during finalize", "job_id", job.ID, "error", err)
		return
	}

	var active []*Job
	for _, j := range jobs {
		if j.ID == job.ID {
			if execErr != nil {
				j.Status = StatusFailed
				j.Error = execErr.Error()
				j.CompletedAt = &now
				*job = *j
				// Keep in .knowledged/queue.json — will be retried on next server start.
				active = append(active, j)
				q.logger.Debug("keeping failed job in .knowledged/queue.json for retry on restart",
					"job_id", j.ID, "error", execErr.Error())
			} else {
				j.Status = StatusDone
				j.Path = path
				j.CompletedAt = &now
				*job = *j
				// Done — remove from .knowledged/queue.json.
				q.logger.Debug("removing completed job from .knowledged/queue.json", "job_id", j.ID)
			}
			continue
		}
		active = append(active, j)
	}

	if err := q.saveJobs(active); err != nil {
		q.logger.Error("failed to save queue during finalize", "job_id", job.ID, "error", err)
	}

	// Cache for GET /jobs/{id} lookups until TTL expires.
	jobCopy := *job
	q.resultsMu.Lock()
	q.results[job.ID] = &jobCopy
	q.resultsMu.Unlock()
	q.logger.Debug("cached job in results map (TTL: 1h)", "job_id", job.ID)
}

// reconcile is called once on startup. It handles any jobs left in a
// non-terminal state from a previous run.
//
//   - done       → loaded into results map, pruned from file
//   - failed     → reset to queued and retried
//   - queued     → re-signal worker
//   - processing → check git log; done if committed, else reset to queued
func (q *Queue) reconcile() error {
	q.mu.Lock()
	defer q.mu.Unlock()

	jobs, err := q.loadJobs()
	if err != nil {
		return fmt.Errorf("loading queue file: %w", err)
	}
	if len(jobs) == 0 {
		q.logEmptyQueueState()
		return nil
	}

	q.logger.Info("reconciling queue on startup", "total_jobs", len(jobs))

	now := time.Now().UTC()
	var active []*Job // only non-terminal jobs go back into .knowledged/queue.json

	for _, job := range jobs {
		switch job.Status {
		case StatusDone:
			// Completed successfully — load into results map and prune from file.
			q.results[job.ID] = job
			q.logger.Debug("pruning completed job from .knowledged/queue.json on startup",
				"job_id", job.ID)

		case StatusFailed:
			// Previous run failed — clear the error and retry.
			q.logger.Info("retrying previously failed job",
				"job_id", job.ID, "previous_error", job.Error)
			job.Status = StatusQueued
			job.Error = ""
			job.CompletedAt = nil
			active = append(active, job)
			select {
			case q.signal <- struct{}{}:
			default:
			}

		case StatusQueued:
			q.logger.Info("found pending job — will retry", "job_id", job.ID)
			active = append(active, job)
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
				active = append(active, job)
			} else if found {
				q.logger.Info("git commit found — marking job done (crash recovery)",
					"job_id", job.ID)
				job.Status = StatusDone
				job.CompletedAt = &now
				q.results[job.ID] = job
				// Terminal — drop from file.
			} else {
				q.logger.Info("no git commit found — resetting job to queued for retry",
					"job_id", job.ID)
				job.Status = StatusQueued
				active = append(active, job)
				select {
				case q.signal <- struct{}{}:
				default:
				}
			}
		}
	}

	if err := q.saveJobs(active); err != nil {
		return fmt.Errorf("saving reconciled queue: %w", err)
	}
	q.logger.Info("queue reconciliation complete",
		"active_jobs", len(active),
		"pruned_terminal_jobs", len(jobs)-len(active))
	return nil
}

func (q *Queue) logEmptyQueueState() {
	info, err := os.Stat(q.path)
	if errors.Is(err, os.ErrNotExist) {
		q.logger.Info("no queue state file found — starting fresh")
		return
	}
	if err != nil {
		q.logger.Warn("could not inspect queue state file after loading zero jobs",
			"path", q.path, "error", err)
		q.logger.Info("no pending jobs found in queue state — starting fresh")
		return
	}
	if info.Size() == 0 {
		q.logger.Info("queue state file is empty — starting fresh", "path", q.path)
		return
	}
	q.logger.Info("no pending jobs found in queue state — starting fresh", "path", q.path)
}

// loadJobs reads .knowledged/queue.json. A missing file is treated as an empty queue.
func (q *Queue) loadJobs() ([]*Job, error) {
	data, err := os.ReadFile(q.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	var jobs []*Job
	if err := json.Unmarshal(data, &jobs); err != nil {
		return nil, fmt.Errorf("parsing .knowledged/queue.json: %w", err)
	}
	return jobs, nil
}

// saveJobs atomically rewrites .knowledged/queue.json.
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
