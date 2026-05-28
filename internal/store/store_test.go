package store

import (
	"bytes"
	"errors"
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

func TestWriteNewFileRejectsExistingFile(t *testing.T) {
	st := newTestStore(t)

	if err := st.WriteFile("notes/hello.md", "original"); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	err := st.WriteNewFile("notes/hello.md", "replacement")
	if !errors.Is(err, ErrFileExists) {
		t.Fatalf("expected ErrFileExists, got %v", err)
	}
	got, err := st.ReadFile("notes/hello.md")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if got != "original" {
		t.Fatalf("existing file was overwritten: %q", got)
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

func TestPushOriginCurrentBranch_RetriesGitHubSSHOver443WhenPort22Blocked(t *testing.T) {
	st := newTestStore(t)

	if err := st.WriteFile("notes/hello.md", "hello world"); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := st.Commit("add hello"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if _, err := st.repo.CreateRemote(&config.RemoteConfig{
		Name: "origin",
		URLs: []string{"git@github.com:subwiz/my-knowledge.git"},
	}); err != nil {
		t.Fatalf("CreateRemote: %v", err)
	}

	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "git.log")
	fakeGit := filepath.Join(binDir, "git")
	if err := os.WriteFile(fakeGit, []byte(`#!/bin/sh
printf '%s\n' "$GIT_SSH_COMMAND" >> "$FAKE_GIT_LOG"
case "$GIT_SSH_COMMAND" in
  *ssh.github.com*Port=443*HostKeyAlias=github.com*) exit 0 ;;
esac
echo "ssh: connect to host github.com port 22: Connection refused" >&2
echo "fatal: Could not read from remote repository." >&2
exit 128
`), 0o755); err != nil {
		t.Fatalf("write fake git: %v", err)
	}
	t.Setenv("FAKE_GIT_LOG", logPath)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if err := st.PushOriginCurrentBranch(); err != nil {
		t.Fatalf("PushOriginCurrentBranch: %v", err)
	}

	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile fake git log: %v", err)
	}
	lines := strings.Split(string(logBytes), "\n")
	if len(lines) != 3 || lines[2] != "" {
		t.Fatalf("expected two git push attempts, got log %q", string(logBytes))
	}
	if lines[0] != "" {
		t.Fatalf("expected first attempt to use default SSH command, got %q", lines[0])
	}
	if !strings.Contains(lines[1], "ssh.github.com") ||
		!strings.Contains(lines[1], "Port=443") ||
		!strings.Contains(lines[1], "HostKeyAlias=github.com") {
		t.Fatalf("expected fallback to force GitHub SSH over port 443 using github.com host key alias, got %q", lines[1])
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
