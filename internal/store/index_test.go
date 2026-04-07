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
