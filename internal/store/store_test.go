package store

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	st, err := New(dir, logger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return st
}

func TestDeleteFile(t *testing.T) {
	st := newTestStore(t)

	// Write a file so there is something to delete.
	if err := st.WriteFile("notes/hello.md", "hello world"); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := st.Commit("add hello"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// File must exist before deletion.
	if !st.FileExists("notes/hello.md") {
		t.Fatal("expected file to exist before delete")
	}

	// Delete it.
	if err := st.DeleteFile("notes/hello.md"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}

	// File must be gone from disk.
	absPath := filepath.Join(st.RepoPath(), "notes", "hello.md")
	if _, err := os.Stat(absPath); !os.IsNotExist(err) {
		t.Errorf("expected file to be absent on disk after DeleteFile, stat err: %v", err)
	}
}

func TestDeleteFile_NotExist(t *testing.T) {
	st := newTestStore(t)

	err := st.DeleteFile("no/such/file.md")
	if err == nil {
		t.Fatal("expected error deleting non-existent file, got nil")
	}
}
