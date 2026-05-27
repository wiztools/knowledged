package tagindex

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wiztools/knowledged/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	st, err := store.New(t.TempDir(), logger)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	return st
}

func writeNote(t *testing.T, st *store.Store, path, title string, tags []string, modified time.Time) {
	t.Helper()
	content := store.RenderFrontmatter(store.Frontmatter{
		Title:       title,
		Description: title + " description",
		Tags:        tags,
		Created:     modified.Add(-time.Hour),
		Modified:    modified,
	}, "body")
	if err := st.WriteFile(path, content); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}

func TestEnsureRebuildsMissingCacheFromFrontmatter(t *testing.T) {
	st := newTestStore(t)
	writeNote(t, st, "tech/go/goroutines.md", "Goroutines", []string{"GoLang", " concurrency ", "golang"}, time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC))
	writeNote(t, st, "infra/docker/setup.md", "Docker Setup", []string{"docker"}, time.Date(2026, 5, 2, 10, 0, 0, 0, time.UTC))

	ti := New(st)
	tags, err := ti.ListTags()
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	if len(tags) != 3 {
		t.Fatalf("expected 3 tags, got %+v", tags)
	}
	if tags[0].Tag != "concurrency" || tags[0].Count != 1 {
		t.Fatalf("unexpected first tag: %+v", tags[0])
	}

	raw, err := os.ReadFile(st.StatePath(stateFile))
	if err != nil {
		t.Fatalf("expected cache file to be written: %v", err)
	}
	var idx fileIndex
	if err := json.Unmarshal(raw, &idx); err != nil {
		t.Fatalf("cache JSON should parse: %v", err)
	}
	if _, ok := idx.Documents["tech/go/goroutines.md"]; !ok {
		t.Fatalf("expected document in cache: %+v", idx.Documents)
	}
}

func TestDocumentsForTagsSupportsAnyAndAll(t *testing.T) {
	st := newTestStore(t)
	writeNote(t, st, "a.md", "A", []string{"go", "api"}, time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC))
	writeNote(t, st, "b.md", "B", []string{"go"}, time.Date(2026, 5, 2, 10, 0, 0, 0, time.UTC))

	ti := New(st)
	anyDocs, err := ti.DocumentsForTags([]string{"go", "api"}, MatchAny)
	if err != nil {
		t.Fatalf("DocumentsForTags any: %v", err)
	}
	if len(anyDocs) != 2 || anyDocs[0].Path != "b.md" || anyDocs[1].Path != "a.md" {
		t.Fatalf("any docs should be newest first, got %+v", anyDocs)
	}

	allDocs, err := ti.DocumentsForTags([]string{"go", "api"}, MatchAll)
	if err != nil {
		t.Fatalf("DocumentsForTags all: %v", err)
	}
	if len(allDocs) != 1 || allDocs[0].Path != "a.md" {
		t.Fatalf("all docs = %+v", allDocs)
	}
}

func TestUpsertAndRemoveDocumentUpdateCache(t *testing.T) {
	st := newTestStore(t)
	ti := New(st)
	writeNote(t, st, "notes/one.md", "One", []string{"old"}, time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC))
	if err := ti.UpsertDocument("notes/one.md"); err != nil {
		t.Fatalf("UpsertDocument old: %v", err)
	}

	writeNote(t, st, "notes/one.md", "One", []string{"new"}, time.Date(2026, 5, 2, 10, 0, 0, 0, time.UTC))
	if err := ti.UpsertDocument("notes/one.md"); err != nil {
		t.Fatalf("UpsertDocument new: %v", err)
	}
	oldDocs, err := ti.DocumentsForTags([]string{"old"}, MatchAny)
	if err != nil {
		t.Fatalf("DocumentsForTags old: %v", err)
	}
	if len(oldDocs) != 0 {
		t.Fatalf("old tag should not retain document: %+v", oldDocs)
	}

	if err := ti.RemoveDocument("notes/one.md"); err != nil {
		t.Fatalf("RemoveDocument: %v", err)
	}
	tags, err := ti.ListTags()
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	if len(tags) != 0 {
		t.Fatalf("expected no tags after removal, got %+v", tags)
	}
	if _, err := os.Stat(filepath.Join(st.RepoPath(), ".knowledged", stateFile)); err != nil {
		t.Fatalf("cache file should still exist: %v", err)
	}
}

func TestListTagsRebuildsStaleCacheWhenHeadChanges(t *testing.T) {
	st := newTestStore(t)
	writeNote(t, st, "notes/one.md", "One", []string{"old"}, time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC))
	if err := st.Commit("seed old"); err != nil {
		t.Fatalf("Commit old: %v", err)
	}
	ti := New(st)
	if _, err := ti.ListTags(); err != nil {
		t.Fatalf("ListTags old: %v", err)
	}

	writeNote(t, st, "notes/one.md", "One", []string{"new"}, time.Date(2026, 5, 2, 10, 0, 0, 0, time.UTC))
	if err := st.Commit("seed new"); err != nil {
		t.Fatalf("Commit new: %v", err)
	}

	tags, err := ti.ListTags()
	if err != nil {
		t.Fatalf("ListTags new: %v", err)
	}
	if len(tags) != 1 || tags[0].Tag != "new" {
		t.Fatalf("expected stale cache rebuild to expose only new tag, got %+v", tags)
	}
}
