package store

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// Store wraps a go-git repository and provides high-level file operations.
// All write methods stage the affected files; call Commit to persist them.
type Store struct {
	repoPath string
	repo     *git.Repository
	worktree *git.Worktree
	logger   *slog.Logger
}

// New opens or initializes the Git repository at repoPath.
//
//   - If repoPath does not exist    → create directory + git init
//   - If repoPath is empty dir      → git init
//   - If repoPath is existing repo  → open
//   - Otherwise                     → error
func New(repoPath string, logger *slog.Logger) (*Store, error) {
	info, err := os.Stat(repoPath)
	if errors.Is(err, os.ErrNotExist) {
		logger.Info("repository path does not exist — creating directory", "path", repoPath)
		if err := os.MkdirAll(repoPath, 0o755); err != nil {
			return nil, fmt.Errorf("creating repo directory: %w", err)
		}
		return initRepo(repoPath, logger)
	}
	if err != nil {
		return nil, fmt.Errorf("stat repo path: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("repo path is not a directory: %s", repoPath)
	}

	// Try to open as existing Git repo.
	repo, err := git.PlainOpen(repoPath)
	if err == nil {
		logger.Info("opened existing Git repository", "path", repoPath)
		wt, err := repo.Worktree()
		if err != nil {
			return nil, fmt.Errorf("getting worktree: %w", err)
		}
		s := &Store{repoPath: repoPath, repo: repo, worktree: wt, logger: logger}
		if err := s.ensureBootstrapped(); err != nil {
			return nil, fmt.Errorf("ensuring repo is bootstrapped: %w", err)
		}
		return s, nil
	}

	if !errors.Is(err, git.ErrRepositoryNotExists) {
		return nil, fmt.Errorf("opening git repo: %w", err)
	}

	// Directory exists but is not a Git repo — only allow if empty.
	entries, err := os.ReadDir(repoPath)
	if err != nil {
		return nil, fmt.Errorf("reading repo directory: %w", err)
	}
	if len(entries) > 0 {
		return nil, fmt.Errorf("directory is not empty and is not a Git repository: %s", repoPath)
	}

	logger.Info("empty directory — initializing Git repository", "path", repoPath)
	return initRepo(repoPath, logger)
}

// initRepo runs git init and creates the initial bootstrap commit.
func initRepo(repoPath string, logger *slog.Logger) (*Store, error) {
	repo, err := git.PlainInit(repoPath, false)
	if err != nil {
		return nil, fmt.Errorf("git init: %w", err)
	}
	logger.Info("initialized new Git repository", "path", repoPath)

	wt, err := repo.Worktree()
	if err != nil {
		return nil, fmt.Errorf("getting worktree: %w", err)
	}

	s := &Store{repoPath: repoPath, repo: repo, worktree: wt, logger: logger}
	if err := s.bootstrap(); err != nil {
		return nil, fmt.Errorf("bootstrapping repo: %w", err)
	}
	return s, nil
}

// bootstrap creates .gitignore and INDEX.md then makes the initial commit.
func (s *Store) bootstrap() error {
	s.logger.Info("bootstrapping knowledge repository")

	gitignore := "queue.json\n"
	if err := os.WriteFile(filepath.Join(s.repoPath, ".gitignore"), []byte(gitignore), 0o644); err != nil {
		return fmt.Errorf("writing .gitignore: %w", err)
	}
	s.logger.Debug("wrote .gitignore", "content", strings.TrimSpace(gitignore))

	indexContent := "# Index\n\n<!-- Auto-managed by knowledged. Do not edit manually. -->\n"
	if err := os.WriteFile(filepath.Join(s.repoPath, "INDEX.md"), []byte(indexContent), 0o644); err != nil {
		return fmt.Errorf("writing INDEX.md: %w", err)
	}
	s.logger.Debug("wrote initial INDEX.md")

	for _, f := range []string{".gitignore", "INDEX.md"} {
		if _, err := s.worktree.Add(f); err != nil {
			return fmt.Errorf("staging %s: %w", f, err)
		}
		s.logger.Debug("staged file", "file", f)
	}

	hash, err := s.worktree.Commit("init: bootstrap knowledge base", &git.CommitOptions{
		Author: signature(),
	})
	if err != nil {
		return fmt.Errorf("initial commit: %w", err)
	}
	s.logger.Info("created initial commit", "hash", hash.String())
	return nil
}

// ensureBootstrapped makes sure INDEX.md and .gitignore exist in an already-
// opened repo (handles the case where someone points at a pre-existing repo).
func (s *Store) ensureBootstrapped() error {
	needsCommit := false

	gitignorePath := filepath.Join(s.repoPath, ".gitignore")
	if _, err := os.Stat(gitignorePath); errors.Is(err, os.ErrNotExist) {
		s.logger.Info("no .gitignore found — creating one", "path", gitignorePath)
		if err := os.WriteFile(gitignorePath, []byte("queue.json\n"), 0o644); err != nil {
			return fmt.Errorf("writing .gitignore: %w", err)
		}
		if _, err := s.worktree.Add(".gitignore"); err != nil {
			return fmt.Errorf("staging .gitignore: %w", err)
		}
		needsCommit = true
	}

	indexPath := filepath.Join(s.repoPath, "INDEX.md")
	if _, err := os.Stat(indexPath); errors.Is(err, os.ErrNotExist) {
		s.logger.Info("no INDEX.md found — creating one", "path", indexPath)
		content := "# Index\n\n<!-- Auto-managed by knowledged. Do not edit manually. -->\n"
		if err := os.WriteFile(indexPath, []byte(content), 0o644); err != nil {
			return fmt.Errorf("writing INDEX.md: %w", err)
		}
		if _, err := s.worktree.Add("INDEX.md"); err != nil {
			return fmt.Errorf("staging INDEX.md: %w", err)
		}
		needsCommit = true
	}

	if needsCommit {
		hash, err := s.worktree.Commit("init: add knowledged bootstrap files", &git.CommitOptions{
			Author: signature(),
		})
		if err != nil {
			return fmt.Errorf("bootstrap commit: %w", err)
		}
		s.logger.Info("created bootstrap commit", "hash", hash.String())
	}
	return nil
}

// RepoPath returns the absolute root path of the repository.
func (s *Store) RepoPath() string { return s.repoPath }

// WriteFile writes content to a path relative to the repo root and stages it.
// Parent directories are created automatically.
func (s *Store) WriteFile(relPath, content string) error {
	absPath := filepath.Join(s.repoPath, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		return fmt.Errorf("creating directories for %s: %w", relPath, err)
	}
	if err := os.WriteFile(absPath, []byte(content), 0o644); err != nil {
		return fmt.Errorf("writing file %s: %w", relPath, err)
	}
	if _, err := s.worktree.Add(filepath.ToSlash(relPath)); err != nil {
		return fmt.Errorf("staging %s: %w", relPath, err)
	}
	s.logger.Debug("wrote and staged file", "path", relPath, "bytes", len(content))
	return nil
}

// ReadFile reads a file at a path relative to the repo root.
func (s *Store) ReadFile(relPath string) (string, error) {
	absPath := filepath.Join(s.repoPath, filepath.FromSlash(relPath))
	data, err := os.ReadFile(absPath)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", relPath, err)
	}
	return string(data), nil
}

// FileExists reports whether a file exists at the given repo-relative path.
func (s *Store) FileExists(relPath string) bool {
	_, err := os.Stat(filepath.Join(s.repoPath, filepath.FromSlash(relPath)))
	return err == nil
}

// MoveFile moves a file within the repo and stages both the addition and removal.
func (s *Store) MoveFile(from, to string) error {
	absFrom := filepath.Join(s.repoPath, filepath.FromSlash(from))
	absTo := filepath.Join(s.repoPath, filepath.FromSlash(to))

	content, err := os.ReadFile(absFrom)
	if err != nil {
		return fmt.Errorf("reading source %s: %w", from, err)
	}
	if err := os.MkdirAll(filepath.Dir(absTo), 0o755); err != nil {
		return fmt.Errorf("creating destination directories: %w", err)
	}
	if err := os.WriteFile(absTo, content, 0o644); err != nil {
		return fmt.Errorf("writing destination %s: %w", to, err)
	}
	if _, err := s.worktree.Add(filepath.ToSlash(to)); err != nil {
		return fmt.Errorf("staging new file %s: %w", to, err)
	}
	// Remove old file from disk and from the index.
	if _, err := s.worktree.Remove(filepath.ToSlash(from)); err != nil {
		return fmt.Errorf("removing old file %s from index: %w", from, err)
	}
	s.logger.Debug("moved file", "from", from, "to", to)
	return nil
}

// DeleteFile removes a file from the repo, stages the removal, and returns an
// error if the file does not exist.
func (s *Store) DeleteFile(relPath string) error {
	if !s.FileExists(relPath) {
		return fmt.Errorf("file not found: %s", relPath)
	}
	if _, err := s.worktree.Remove(filepath.ToSlash(relPath)); err != nil {
		return fmt.Errorf("removing %s from index: %w", relPath, err)
	}
	s.logger.Debug("deleted and staged removal", "path", relPath)
	return nil
}

// Commit creates a Git commit with all currently staged changes.
func (s *Store) Commit(message string) error {
	hash, err := s.worktree.Commit(message, &git.CommitOptions{
		Author: signature(),
	})
	if err != nil {
		return fmt.Errorf("committing: %w", err)
	}
	s.logger.Info("created commit", "hash", hash.String(), "message", message)
	return nil
}

// FindCommitByJobID searches recent git history for a commit whose message
// contains jobID. Used for crash-recovery reconciliation.
func (s *Store) FindCommitByJobID(jobID string) (bool, error) {
	iter, err := s.repo.Log(&git.LogOptions{})
	if err != nil {
		// No commits yet — job definitely not committed.
		return false, nil
	}
	defer iter.Close()

	found := false
	iterErr := iter.ForEach(func(c *object.Commit) error {
		if strings.Contains(c.Message, jobID) {
			found = true
			return fmt.Errorf("stop") // signal early exit
		}
		return nil
	})
	// We use a sentinel string error to break early; ignore it.
	if iterErr != nil && iterErr.Error() != "stop" {
		return false, fmt.Errorf("iterating commits: %w", iterErr)
	}
	return found, nil
}

func signature() *object.Signature {
	return &object.Signature{
		Name:  "knowledged",
		Email: "knowledged@local",
		When:  time.Now(),
	}
}
