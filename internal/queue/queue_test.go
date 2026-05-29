package queue

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/wiztools/knowledged/internal/llm"
	"github.com/wiztools/knowledged/internal/organizer"
	"github.com/wiztools/knowledged/internal/store"
)

type fakeLLM struct {
	replies []string
	calls   []fakeCall
}

type fakeCall struct {
	user string
}

func (f *fakeLLM) Complete(_ context.Context, _, user string, _ ...llm.CallOption) (string, error) {
	f.calls = append(f.calls, fakeCall{user: user})
	return f.next()
}

func (f *fakeLLM) CompleteStructured(_ context.Context, _, user string, _ llm.Schema, _ ...llm.CallOption) (string, error) {
	f.calls = append(f.calls, fakeCall{user: user})
	return f.next()
}

func (f *fakeLLM) next() (string, error) {
	if len(f.calls) > len(f.replies) {
		return "", errors.New("fakeLLM: ran out of canned replies")
	}
	return f.replies[len(f.calls)-1], nil
}

func newTestQueue(t *testing.T) (*Queue, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	st, err := store.New(dir, logger)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	// organizer is nil — worker is never started in these tests.
	q, err := New(st, nil, nil, nil, logger, 0)
	if err != nil {
		t.Fatalf("queue.New: %v", err)
	}
	return q, st
}

func TestEnqueue_DeduplicatesActiveJobs(t *testing.T) {
	q, _ := newTestQueue(t)

	content := "some knowledge content"
	first, err := q.Enqueue(content, "hint", []string{"tag"})
	if err != nil {
		t.Fatalf("first Enqueue: %v", err)
	}

	second, err := q.Enqueue(content, "hint", []string{"tag"})
	if err != nil {
		t.Fatalf("second Enqueue: %v", err)
	}

	if first.ID != second.ID {
		t.Errorf("expected duplicate enqueue to return same job ID %q, got %q", first.ID, second.ID)
	}

	jobs, err := q.loadJobs()
	if err != nil {
		t.Fatalf("loadJobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Errorf("expected 1 job in queue after duplicate enqueue, got %d", len(jobs))
	}
}

func TestEnqueue_AllowsRepostAfterCompletion(t *testing.T) {
	q, _ := newTestQueue(t)

	content := "some knowledge content"
	first, err := q.Enqueue(content, "hint", nil)
	if err != nil {
		t.Fatalf("first Enqueue: %v", err)
	}

	// Simulate job completing: remove from queue, cache as done.
	q.mu.Lock()
	if err := q.saveJobs(nil); err != nil {
		q.mu.Unlock()
		t.Fatalf("saveJobs: %v", err)
	}
	q.mu.Unlock()
	now := time.Now().UTC()
	first.Status = StatusDone
	first.CompletedAt = &now
	q.resultsMu.Lock()
	q.results[first.ID] = first
	q.resultsMu.Unlock()

	second, err := q.Enqueue(content, "hint", nil)
	if err != nil {
		t.Fatalf("second Enqueue after completion: %v", err)
	}

	if second.ID == first.ID {
		t.Error("expected a new job ID after the original job completed, got the same ID")
	}
}

func TestEnqueue_ContentHashSet(t *testing.T) {
	q, _ := newTestQueue(t)

	job, err := q.Enqueue("hello world", "", nil)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if job.ContentHash == "" {
		t.Error("expected ContentHash to be set on enqueued job")
	}
	if job.ContentHash != contentHash("hello world") {
		t.Errorf("ContentHash mismatch: got %q", job.ContentHash)
	}
}

func TestProcessJobRetriesPlacementOnExistingTargetPath(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	st, err := store.New(dir, logger)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	existing := store.RenderFrontmatter(store.Frontmatter{
		Title:       "Go Goroutines",
		Description: "concurrency primitives",
		Created:     time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC),
		Modified:    time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC),
	}, "original body")
	if err := st.WriteFile("tech/go/goroutines.md", existing); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	index := `# Index

<!-- Auto-managed by knowledged. Do not edit manually. -->

## Go
- [Go Goroutines](tech/go/goroutines.md) — concurrency primitives
`
	if err := st.WriteIndex(index); err != nil {
		t.Fatalf("WriteIndex: %v", err)
	}
	if err := st.Commit("seed"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	llm := &fakeLLM{replies: []string{
		`{"candidate_sections":["Go"],"proposed_new_section":""}`,
		`{"target_path":"tech/go/goroutines.md","title":"Go Generics","description":"Type parameters","refactors":[],"updated_sections":[{"name":"Go","body":"- [Go Goroutines](tech/go/goroutines.md) — concurrency primitives\n- [Go Generics](tech/go/goroutines.md) — type parameters\n"}]}`,
		`{"candidate_sections":["Go"],"proposed_new_section":""}`,
		`{"target_path":"tech/go/generics.md","title":"Go Generics","description":"Type parameters","refactors":[],"updated_sections":[{"name":"Go","body":"- [Go Goroutines](tech/go/goroutines.md) — concurrency primitives\n- [Go Generics](tech/go/generics.md) — type parameters\n"}]}`,
	}}
	org := organizer.New(st, llm, logger)
	q, err := New(st, org, nil, nil, logger, 0)
	if err != nil {
		t.Fatalf("queue.New: %v", err)
	}
	job, err := q.Enqueue("Go supports type parameters.", "go generics", nil)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	q.processJob(context.Background(), job)

	if job.Status != StatusDone {
		t.Fatalf("expected job done after retry, got %s: %s", job.Status, job.Error)
	}
	gotExisting, err := st.ReadFile("tech/go/goroutines.md")
	if err != nil {
		t.Fatalf("ReadFile existing: %v", err)
	}
	if gotExisting != existing {
		t.Fatalf("existing content was overwritten:\n%s", gotExisting)
	}
	if !st.FileExists("tech/go/generics.md") {
		t.Fatal("expected retried placement to create tech/go/generics.md")
	}
	if len(llm.calls) != 4 {
		t.Fatalf("expected 4 LLM calls, got %d", len(llm.calls))
	}
	if !strings.Contains(llm.calls[2].user, "tech/go/goroutines.md") {
		t.Fatalf("retry route prompt missing conflicting path:\n%s", llm.calls[2].user)
	}
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

func TestEnqueueEdit_JobFields(t *testing.T) {
	q, _ := newTestQueue(t)

	job, err := q.EnqueueEdit("tech/go/goroutines.md", "updated", "Goroutines", "runtime notes", nil)
	if err != nil {
		t.Fatalf("EnqueueEdit: %v", err)
	}

	if job.Operation != "edit" {
		t.Errorf("expected Operation %q, got %q", "edit", job.Operation)
	}
	if job.Path != "tech/go/goroutines.md" {
		t.Errorf("expected Path %q, got %q", "tech/go/goroutines.md", job.Path)
	}
	if job.Content != "updated" {
		t.Errorf("expected Content to be set, got %q", job.Content)
	}
	if job.Title != "Goroutines" || job.Description != "runtime notes" {
		t.Errorf("expected index metadata fields to be set, got title=%q description=%q", job.Title, job.Description)
	}
	if job.ContentHash == "" {
		t.Error("expected ContentHash to be set")
	}
}

func TestExecuteEdit_UpdatesFileIndexAndCommit(t *testing.T) {
	q, st := newTestQueue(t)

	existing := store.RenderFrontmatter(store.Frontmatter{
		Title:       "Go Goroutines",
		Description: "concurrency primitives",
		Tags:        []string{},
		Created:     time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC),
		Modified:    time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC),
	}, "old content")
	if err := st.WriteFile("tech/go/goroutines.md", existing); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	index := `# Index

<!-- Auto-managed by knowledged. Do not edit manually. -->

- [Go Goroutines](tech/go/goroutines.md) — concurrency primitives
`
	if err := st.WriteIndex(index); err != nil {
		t.Fatalf("WriteIndex: %v", err)
	}
	if err := st.Commit("seed"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	job, err := q.EnqueueEdit("tech/go/goroutines.md", "new content", "Go Scheduler", "updated runtime notes", nil)
	if err != nil {
		t.Fatalf("EnqueueEdit: %v", err)
	}

	q.executeEdit(context.Background(), job)

	if job.Status != StatusDone {
		t.Fatalf("expected edit job done, got %s: %s", job.Status, job.Error)
	}
	got, err := st.ReadFile("tech/go/goroutines.md")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	fm, body, err := store.ParseFrontmatter(got)
	if err != nil {
		t.Fatalf("ParseFrontmatter: %v", err)
	}
	if fm.Title != "Go Scheduler" || fm.Description != "updated runtime notes" {
		t.Fatalf("frontmatter = %#v", fm)
	}
	if body != "new content" {
		t.Fatalf("expected edited body, got %q", body)
	}
	gotIndex, err := st.ReadIndex()
	if err != nil {
		t.Fatalf("ReadIndex: %v", err)
	}
	if !strings.Contains(gotIndex, "- [Go Scheduler](tech/go/goroutines.md) — updated runtime notes") {
		t.Fatalf("expected updated index entry, got:\n%s", gotIndex)
	}
	found, err := st.FindCommitByJobID(job.ID)
	if err != nil {
		t.Fatalf("FindCommitByJobID: %v", err)
	}
	if !found {
		t.Fatalf("expected git commit containing job id %s", job.ID)
	}
}

func TestExecuteEdit_MetadataOnlyPreservesBodyAndUpdatesTags(t *testing.T) {
	q, st := newTestQueue(t)

	created := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	existing := store.RenderFrontmatter(store.Frontmatter{
		Title:       "Old Title",
		Description: "Old description",
		Tags:        []string{"old"},
		Created:     created,
		Modified:    created,
	}, "# Old Title\n\nBody.\n")
	if err := st.WriteFile("notes/hello.md", existing); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := st.Commit("seed"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	job, err := q.EnqueueEdit("notes/hello.md", "", "New Title", "", []string{"new", "tag"})
	if err != nil {
		t.Fatalf("EnqueueEdit: %v", err)
	}

	q.executeEdit(context.Background(), job)

	if job.Status != StatusDone {
		t.Fatalf("expected edit job done, got %s: %s", job.Status, job.Error)
	}
	got, err := st.ReadFile("notes/hello.md")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	fm, body, err := store.ParseFrontmatter(got)
	if err != nil {
		t.Fatalf("ParseFrontmatter: %v", err)
	}
	if fm.Title != "New Title" || fm.Description != "Old description" {
		t.Fatalf("frontmatter = %#v", fm)
	}
	if gotTags, want := strings.Join(fm.Tags, ","), "new,tag"; gotTags != want {
		t.Fatalf("tags = %q, want %q", gotTags, want)
	}
	if body != "# Old Title\n\nBody.\n" {
		t.Fatalf("body = %q", body)
	}
}

func TestExecuteEdit_MissingFileFails(t *testing.T) {
	q, _ := newTestQueue(t)

	job, err := q.EnqueueEdit("missing/file.md", "new content", "", "", nil)
	if err != nil {
		t.Fatalf("EnqueueEdit: %v", err)
	}

	q.executeEdit(context.Background(), job)

	if job.Status != StatusFailed {
		t.Fatalf("expected edit job failed, got %s", job.Status)
	}
	if !strings.Contains(job.Error, "file not found") {
		t.Fatalf("expected file not found error, got %q", job.Error)
	}
}

func TestPushOriginCurrentBranch_LogsErrorOnFailure(t *testing.T) {
	dir := t.TempDir()
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	if _, err := repo.CreateRemote(&config.RemoteConfig{
		Name: "origin",
		URLs: []string{"/path/that/does/not/exist"},
	}); err != nil {
		t.Fatalf("CreateRemote: %v", err)
	}

	st, err := store.New(dir, logger)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	if err := st.WriteFile("notes/hello.md", "hello world"); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := st.Commit("add hello"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	q, err := New(st, nil, nil, nil, logger, 24*time.Hour)
	if err != nil {
		t.Fatalf("queue.New: %v", err)
	}

	q.pushOriginCurrentBranch(time.Now())

	out := logs.String()
	if !strings.Contains(out, "level=ERROR") {
		t.Fatalf("expected error log, got %q", out)
	}
	if !strings.Contains(out, "periodic git push started") {
		t.Fatalf("expected periodic push start log message, got %q", out)
	}
	if !strings.Contains(out, "periodic git push failed") {
		t.Fatalf("expected periodic push log message, got %q", out)
	}
	if !strings.Contains(out, "duration=") {
		t.Fatalf("expected periodic push duration, got %q", out)
	}
}

func TestRunScheduledPush_LogsSkippedWithoutOrigin(t *testing.T) {
	dir := t.TempDir()
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	if _, err := git.PlainInit(dir, false); err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	st, err := store.New(dir, logger)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	q, err := New(st, nil, nil, nil, logger, 15*time.Minute)
	if err != nil {
		t.Fatalf("queue.New: %v", err)
	}

	q.runScheduledPush()

	out := logs.String()
	if !strings.Contains(out, "periodic git push skipped") {
		t.Fatalf("expected periodic push skip log message, got %q", out)
	}
	if !strings.Contains(out, "reason=\"origin remote not configured\"") {
		t.Fatalf("expected skip reason, got %q", out)
	}
}

func TestNextPushDelay_UsesPersistedLastAttempt(t *testing.T) {
	q, _ := newTestQueue(t)
	q.pushOriginEvery = 24 * time.Hour

	now := time.Date(2026, 4, 12, 10, 0, 0, 0, time.UTC)
	if err := q.writeLastOriginPushAttempt(now.Add(-23 * time.Hour)); err != nil {
		t.Fatalf("writeLastOriginPushAttempt: %v", err)
	}

	delay, err := q.nextPushDelay(now)
	if err != nil {
		t.Fatalf("nextPushDelay: %v", err)
	}
	if delay != time.Hour {
		t.Fatalf("expected 1h delay, got %s", delay)
	}
}

func TestNextPushDelay_DueImmediatelyWhenMissingOrExpired(t *testing.T) {
	q, _ := newTestQueue(t)
	q.pushOriginEvery = 24 * time.Hour

	now := time.Date(2026, 4, 12, 10, 0, 0, 0, time.UTC)
	delay, err := q.nextPushDelay(now)
	if err != nil {
		t.Fatalf("nextPushDelay without state: %v", err)
	}
	if delay != 0 {
		t.Fatalf("expected immediate delay without state, got %s", delay)
	}

	if err := q.writeLastOriginPushAttempt(now.Add(-25 * time.Hour)); err != nil {
		t.Fatalf("writeLastOriginPushAttempt: %v", err)
	}
	delay, err = q.nextPushDelay(now)
	if err != nil {
		t.Fatalf("nextPushDelay expired: %v", err)
	}
	if delay != 0 {
		t.Fatalf("expected immediate delay when expired, got %s", delay)
	}
}

func TestLogEmptyQueueState_Messages(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(t *testing.T, q *Queue)
		want    string
	}{
		{
			name: "missing file",
			prepare: func(t *testing.T, q *Queue) {
				t.Helper()
			},
			want: "no queue state file found",
		},
		{
			name: "empty file",
			prepare: func(t *testing.T, q *Queue) {
				t.Helper()
				if err := os.WriteFile(q.path, nil, 0o644); err != nil {
					t.Fatalf("WriteFile empty queue: %v", err)
				}
			},
			want: "queue state file is empty",
		},
		{
			name: "empty array",
			prepare: func(t *testing.T, q *Queue) {
				t.Helper()
				if err := os.WriteFile(q.path, []byte("[]"), 0o644); err != nil {
					t.Fatalf("WriteFile empty array queue: %v", err)
				}
			},
			want: "no pending jobs found in queue state",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			st, err := store.New(t.TempDir(), logger)
			if err != nil {
				t.Fatalf("store.New: %v", err)
			}
			q, err := New(st, nil, nil, nil, logger, 0)
			if err != nil {
				t.Fatalf("queue.New: %v", err)
			}

			if err := os.Remove(q.path); err != nil && !os.IsNotExist(err) {
				t.Fatalf("Remove queue file: %v", err)
			}
			tc.prepare(t, q)
			logs.Reset()

			q.logEmptyQueueState()

			if !strings.Contains(logs.String(), tc.want) {
				t.Fatalf("expected log containing %q, got %q", tc.want, logs.String())
			}
		})
	}
}
