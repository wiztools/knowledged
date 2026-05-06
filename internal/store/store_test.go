package store

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
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

func TestCleanContentPathRejectsTraversal(t *testing.T) {
	bad := []string{
		"../outside.md",
		"/tmp/outside.md",
		".knowledged/queue.json",
		"notes/plain.txt",
	}
	for _, path := range bad {
		if _, err := CleanContentPath(path); err == nil {
			t.Fatalf("expected %q to be rejected", path)
		}
	}
}

func TestWriteFileRejectsTraversal(t *testing.T) {
	st := newTestStore(t)

	if err := st.WriteFile("../outside.md", "escape"); err == nil {
		t.Fatal("expected traversal path to be rejected")
	}
	if _, err := os.Stat(filepath.Join(st.RepoPath(), "..", "outside.md")); !os.IsNotExist(err) {
		t.Fatalf("expected outside file not to be created, stat err: %v", err)
	}
}

func TestPushOriginCurrentBranch(t *testing.T) {
	st := newTestStore(t)

	if err := st.WriteFile("notes/hello.md", "hello world"); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := st.Commit("add hello"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	remoteDir := t.TempDir()
	if _, err := git.PlainInit(remoteDir, true); err != nil {
		t.Fatalf("PlainInit remote: %v", err)
	}
	if _, err := st.repo.CreateRemote(&config.RemoteConfig{
		Name: "origin",
		URLs: []string{remoteDir},
	}); err != nil {
		t.Fatalf("CreateRemote: %v", err)
	}

	head, err := st.repo.Head()
	if err != nil {
		t.Fatalf("repo.Head: %v", err)
	}

	if err := st.PushOriginCurrentBranch(); err != nil {
		t.Fatalf("PushOriginCurrentBranch: %v", err)
	}

	remoteRepo, err := git.PlainOpen(remoteDir)
	if err != nil {
		t.Fatalf("PlainOpen remote: %v", err)
	}
	remoteRef, err := remoteRepo.Reference(head.Name(), true)
	if err != nil {
		t.Fatalf("remote reference: %v", err)
	}
	if remoteRef.Hash() != head.Hash() {
		t.Fatalf("expected remote hash %s, got %s", head.Hash(), remoteRef.Hash())
	}
}

func TestNew_CreatesHiddenStateDirAndIgnoresIt(t *testing.T) {
	dir := t.TempDir()
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelError}))

	st, err := New(dir, logger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if info, err := os.Stat(filepath.Join(st.RepoPath(), ".knowledged")); err != nil {
		t.Fatalf("stat .knowledged: %v", err)
	} else if !info.IsDir() {
		t.Fatalf(".knowledged exists but is not a directory")
	}

	gitignore, err := os.ReadFile(filepath.Join(st.RepoPath(), ".gitignore"))
	if err != nil {
		t.Fatalf("ReadFile .gitignore: %v", err)
	}
	if !strings.Contains(string(gitignore), "/.knowledged/") {
		t.Fatalf("expected .gitignore to ignore only the root .knowledged/, got %q", string(gitignore))
	}
}
