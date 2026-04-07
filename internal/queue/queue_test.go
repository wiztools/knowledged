package queue

import (
	"log/slog"
	"os"
	"testing"

	"github.com/wiztools/knowledged/internal/store"
)

func newTestQueue(t *testing.T) (*Queue, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	st, err := store.New(dir, logger)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	// organizer is nil — worker is never started in these tests.
	q, err := New(st, nil, logger)
	if err != nil {
		t.Fatalf("queue.New: %v", err)
	}
	return q, st
}

func TestEnqueueDelete_JobFields(t *testing.T) {
	q, _ := newTestQueue(t)

	job, err := q.EnqueueDelete("tech/go/goroutines.md")
	if err != nil {
		t.Fatalf("EnqueueDelete: %v", err)
	}

	if job.Operation != "delete" {
		t.Errorf("expected Operation %q, got %q", "delete", job.Operation)
	}
	if job.Path != "tech/go/goroutines.md" {
		t.Errorf("expected Path %q, got %q", "tech/go/goroutines.md", job.Path)
	}
	if job.Status != StatusQueued {
		t.Errorf("expected status %q, got %q", StatusQueued, job.Status)
	}
	if job.ID == "" {
		t.Error("expected non-empty job ID")
	}
}

func TestEnqueueDelete_Retrievable(t *testing.T) {
	q, _ := newTestQueue(t)

	job, err := q.EnqueueDelete("notes/meeting.md")
	if err != nil {
		t.Fatalf("EnqueueDelete: %v", err)
	}

	got, ok := q.GetJob(job.ID)
	if !ok {
		t.Fatalf("GetJob returned not found for job %s", job.ID)
	}
	if got.Operation != "delete" {
		t.Errorf("expected Operation %q after GetJob, got %q", "delete", got.Operation)
	}
}
