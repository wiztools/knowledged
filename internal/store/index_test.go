package store

import (
	"strings"
	"testing"
)

func TestRemoveIndexEntry(t *testing.T) {
	st := newTestStore(t)

	index := `# Index

<!-- Auto-managed by knowledged. Do not edit manually. -->

- [Go Goroutines](tech/go/goroutines.md) — concurrency primitives
- [Rust Ownership](lang/rust/ownership.md) — memory safety model
- [Docker Setup](infra/docker/setup.md) — container basics
`
	if err := st.WriteIndex(index); err != nil {
		t.Fatalf("WriteIndex: %v", err)
	}
	if err := st.Commit("add index"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	if err := st.RemoveIndexEntry("lang/rust/ownership.md"); err != nil {
		t.Fatalf("RemoveIndexEntry: %v", err)
	}

	got, err := st.ReadIndex()
	if err != nil {
		t.Fatalf("ReadIndex: %v", err)
	}

	if strings.Contains(got, "lang/rust/ownership.md") {
		t.Errorf("expected entry to be removed, still present:\n%s", got)
	}
	if !strings.Contains(got, "tech/go/goroutines.md") {
		t.Errorf("expected other entries to remain:\n%s", got)
	}
	if !strings.Contains(got, "infra/docker/setup.md") {
		t.Errorf("expected other entries to remain:\n%s", got)
	}
}

func TestRemoveIndexEntry_NotInIndex(t *testing.T) {
	st := newTestStore(t)
	// RemoveIndexEntry for a path not in the index should be a no-op (not an error).
	if err := st.RemoveIndexEntry("does/not/exist.md"); err != nil {
		t.Fatalf("expected no error for missing index entry, got: %v", err)
	}
}

func TestUpdateIndexEntry(t *testing.T) {
	st := newTestStore(t)

	index := `# Index

<!-- Auto-managed by knowledged. Do not edit manually. -->

- [Go Goroutines](tech/go/goroutines.md) — concurrency primitives
- [Rust Ownership](lang/rust/ownership.md) — memory safety model
`
	if err := st.WriteIndex(index); err != nil {
		t.Fatalf("WriteIndex: %v", err)
	}

	if err := st.UpdateIndexEntry("tech/go/goroutines.md", "Go Scheduler", "updated runtime notes"); err != nil {
		t.Fatalf("UpdateIndexEntry: %v", err)
	}

	got, err := st.ReadIndex()
	if err != nil {
		t.Fatalf("ReadIndex: %v", err)
	}
	if !strings.Contains(got, "- [Go Scheduler](tech/go/goroutines.md) — updated runtime notes") {
		t.Fatalf("expected updated entry, got:\n%s", got)
	}
	if !strings.Contains(got, "lang/rust/ownership.md") {
		t.Fatalf("expected other entries to remain, got:\n%s", got)
	}
}

func TestUpdateIndexEntry_PreservesEmptyFields(t *testing.T) {
	st := newTestStore(t)

	index := `# Index

<!-- Auto-managed by knowledged. Do not edit manually. -->

- [Go Goroutines](tech/go/goroutines.md) — concurrency primitives
`
	if err := st.WriteIndex(index); err != nil {
		t.Fatalf("WriteIndex: %v", err)
	}

	if err := st.UpdateIndexEntry("tech/go/goroutines.md", "", "new description"); err != nil {
		t.Fatalf("UpdateIndexEntry: %v", err)
	}

	got, err := st.ReadIndex()
	if err != nil {
		t.Fatalf("ReadIndex: %v", err)
	}
	if !strings.Contains(got, "- [Go Goroutines](tech/go/goroutines.md) — new description") {
		t.Fatalf("expected title to be preserved, got:\n%s", got)
	}
}

func TestRebuildIndex(t *testing.T) {
	got := RebuildIndex([]NoteWithFrontmatter{
		{
			Path: "ai/llm/gguf.md",
			Frontmatter: Frontmatter{
				Title:       "GGUF Models",
				Description: "Local model format",
			},
		},
		{
			Path: "notes/root.md",
			Frontmatter: Frontmatter{
				Title: "Root Note",
			},
		},
		{
			Path: "ai/rag.md",
			Frontmatter: Frontmatter{
				Title:       "RAG",
				Description: "Retrieval notes",
			},
		},
	})

	want := `# Index

<!-- Auto-managed by knowledged. Do not edit manually. -->

## AI
- [GGUF Models](ai/llm/gguf.md) — Local model format
- [RAG](ai/rag.md) — Retrieval notes

## Notes
- [Root Note](notes/root.md)
`
	if got != want {
		t.Fatalf("RebuildIndex mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestRebuildIndex_SkipsInvalidAndUntitledNotes(t *testing.T) {
	got := RebuildIndex([]NoteWithFrontmatter{
		{Path: "INDEX.md", Frontmatter: Frontmatter{Title: "Index"}},
		{Path: ".knowledged/state.md", Frontmatter: Frontmatter{Title: "State"}},
		{Path: "notes/untitled.md", Frontmatter: Frontmatter{}},
		{Path: "source-materials/web.md", Frontmatter: Frontmatter{Title: "Web Sources"}},
	})

	if strings.Contains(got, "](INDEX.md)") || strings.Contains(got, "](.knowledged/state.md)") || strings.Contains(got, "untitled") {
		t.Fatalf("RebuildIndex included skipped notes:\n%s", got)
	}
	if !strings.Contains(got, "## Source Materials\n- [Web Sources](source-materials/web.md)\n") {
		t.Fatalf("RebuildIndex did not render humanized section:\n%s", got)
	}
}
