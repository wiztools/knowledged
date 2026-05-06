package store

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
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

const stateDirName = ".knowledged"
const rootedStateDirPattern = "/" + stateDirName + "/"

// CleanContentPath validates a user-supplied repository-relative Markdown path
// and returns it in slash-separated form.
func CleanContentPath(relPath string) (string, error) {
	p := strings.TrimSpace(filepath.ToSlash(relPath))
	if p == "" {
		return "", fmt.Errorf("path must not be empty")
	}
	if filepath.IsAbs(p) || strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("path must be repo-relative: %s", relPath)
	}
	clean := filepath.ToSlash(filepath.Clean(p))
	if clean == "." || clean == "" {
		return "", fmt.Errorf("path must not be empty")
	}
	if clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") {
		return "", fmt.Errorf("path must stay within the repository: %s", relPath)
	}
	if clean == stateDirName || strings.HasPrefix(clean, stateDirName+"/") {
		return "", fmt.Errorf("path must not target %s", stateDirName)
	}
	if clean != indexFile && !strings.HasSuffix(strings.ToLower(clean), ".md") {
		return "", fmt.Errorf("path must be a Markdown file: %s", relPath)
	}
	return clean, nil
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

	if err := s.ensureStateDir(); err != nil {
		return err
	}

	gitignore := rootedStateDirPattern + "\n"
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

	if err := s.ensureStateDir(); err != nil {
		return err
	}

	gitignorePath := filepath.Join(s.repoPath, ".gitignore")
	if _, err := os.Stat(gitignorePath); errors.Is(err, os.ErrNotExist) {
		s.logger.Info("no .gitignore found — creating one", "path", gitignorePath)
		if err := os.WriteFile(gitignorePath, []byte(rootedStateDirPattern+"\n"), 0o644); err != nil {
			return fmt.Errorf("writing .gitignore: %w", err)
		}
		if _, err := s.worktree.Add(".gitignore"); err != nil {
			return fmt.Errorf("staging .gitignore: %w", err)
		}
		needsCommit = true
	} else if err != nil {
		return fmt.Errorf("stat .gitignore: %w", err)
	} else {
		content, err := os.ReadFile(gitignorePath)
		if err != nil {
			return fmt.Errorf("reading .gitignore: %w", err)
		}
		if !strings.Contains(string(content), rootedStateDirPattern) {
			updated := strings.TrimRight(string(content), "\n") + "\n" + rootedStateDirPattern + "\n"
			if err := os.WriteFile(gitignorePath, []byte(updated), 0o644); err != nil {
				return fmt.Errorf("updating .gitignore: %w", err)
			}
			if _, err := s.worktree.Add(".gitignore"); err != nil {
				return fmt.Errorf("staging .gitignore: %w", err)
			}
			needsCommit = true
		}
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

// StatePath returns the absolute path for an operational state file stored
// under the repo-local hidden state directory.
func (s *Store) StatePath(name string) string {
	return filepath.Join(s.repoPath, stateDirName, name)
}

// WriteFile writes content to a path relative to the repo root and stages it.
// Parent directories are created automatically.
func (s *Store) WriteFile(relPath, content string) error {
	cleanPath, err := CleanContentPath(relPath)
	if err != nil {
		return err
	}
	absPath := filepath.Join(s.repoPath, filepath.FromSlash(cleanPath))
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		return fmt.Errorf("creating directories for %s: %w", cleanPath, err)
	}
	if err := os.WriteFile(absPath, []byte(content), 0o644); err != nil {
		return fmt.Errorf("writing file %s: %w", cleanPath, err)
	}
	if _, err := s.worktree.Add(cleanPath); err != nil {
		return fmt.Errorf("staging %s: %w", cleanPath, err)
	}
	s.logger.Debug("wrote and staged file", "path", cleanPath, "bytes", len(content))
	return nil
}

// ReadFile reads a file at a path relative to the repo root.
func (s *Store) ReadFile(relPath string) (string, error) {
	cleanPath, err := CleanContentPath(relPath)
	if err != nil {
		return "", err
	}
	absPath := filepath.Join(s.repoPath, filepath.FromSlash(cleanPath))
	data, err := os.ReadFile(absPath)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", cleanPath, err)
	}
	return string(data), nil
}

// FileExists reports whether a file exists at the given repo-relative path.
func (s *Store) FileExists(relPath string) bool {
	cleanPath, err := CleanContentPath(relPath)
	if err != nil {
		return false
	}
	_, err = os.Stat(filepath.Join(s.repoPath, filepath.FromSlash(cleanPath)))
	return err == nil
}

// MoveFile moves a file within the repo and stages both the addition and removal.
func (s *Store) MoveFile(from, to string) error {
	cleanFrom, err := CleanContentPath(from)
	if err != nil {
		return err
	}
	cleanTo, err := CleanContentPath(to)
	if err != nil {
		return err
	}
	absFrom := filepath.Join(s.repoPath, filepath.FromSlash(cleanFrom))
	absTo := filepath.Join(s.repoPath, filepath.FromSlash(cleanTo))

	content, err := os.ReadFile(absFrom)
	if err != nil {
		return fmt.Errorf("reading source %s: %w", cleanFrom, err)
	}
	if err := os.MkdirAll(filepath.Dir(absTo), 0o755); err != nil {
		return fmt.Errorf("creating destination directories: %w", err)
	}
	if err := os.WriteFile(absTo, content, 0o644); err != nil {
		return fmt.Errorf("writing destination %s: %w", cleanTo, err)
	}
	if _, err := s.worktree.Add(cleanTo); err != nil {
		return fmt.Errorf("staging new file %s: %w", cleanTo, err)
	}
	// Remove old file from disk and from the index.
	if _, err := s.worktree.Remove(cleanFrom); err != nil {
		return fmt.Errorf("removing old file %s from index: %w", cleanFrom, err)
	}
	s.logger.Debug("moved file", "from", cleanFrom, "to", cleanTo)
	return nil
}

// DeleteFile removes a file from the repo, stages the removal, and returns an
// error if the file does not exist.
func (s *Store) DeleteFile(relPath string) error {
	cleanPath, err := CleanContentPath(relPath)
	if err != nil {
		return err
	}
	if !s.FileExists(cleanPath) {
		return fmt.Errorf("file not found: %s", cleanPath)
	}
	if _, err := s.worktree.Remove(cleanPath); err != nil {
		return fmt.Errorf("removing %s from index: %w", cleanPath, err)
	}
	s.logger.Debug("deleted and staged removal", "path", cleanPath)
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

// HasOriginRemote reports whether the repository has an origin remote.
func (s *Store) HasOriginRemote() (bool, error) {
	if _, err := s.repo.Remote("origin"); err != nil {
		if errors.Is(err, git.ErrRemoteNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("looking up origin remote: %w", err)
	}
	return true, nil
}

// PushOriginCurrentBranch pushes the currently checked-out branch to origin.
// If no origin remote is configured, it is a no-op. It tries go-git first for
// HTTPS remotes and falls back to the git CLI (which uses system SSH auth
// helpers) for SSH remotes or when go-git fails.
func (s *Store) PushOriginCurrentBranch() error {
	hasOrigin, err := s.HasOriginRemote()
	if err != nil {
		return err
	}
	if !hasOrigin {
		return nil
	}

	head, err := s.repo.Head()
	if err != nil {
		return fmt.Errorf("resolving HEAD: %w", err)
	}
	if !head.Name().IsBranch() {
		return fmt.Errorf("HEAD is not on a branch: %s", head.Name())
	}

	remote, err := s.repo.Remote("origin")
	if err != nil {
		return fmt.Errorf("looking up origin remote: %w", err)
	}
	urls := remote.Config().URLs
	isSSH := len(urls) > 0 && !strings.HasPrefix(urls[0], "http://") && !strings.HasPrefix(urls[0], "https://")

	if !isSSH {
		refSpec := config.RefSpec(head.Name().String() + ":" + head.Name().String())
		if err := s.repo.Push(&git.PushOptions{
			RemoteName: "origin",
			RefSpecs:   []config.RefSpec{refSpec},
		}); err == nil || errors.Is(err, git.NoErrAlreadyUpToDate) {
			return nil
		} else {
			s.logger.Debug("go-git push failed, falling back to git CLI", "error", err)
		}
	}

	out, execErr := exec.Command("git", "-C", s.repoPath, "push", "--porcelain", "origin", head.Name().Short()).CombinedOutput()
	if execErr != nil {
		msg := strings.ReplaceAll(strings.TrimSpace(string(out)), "\r\n", "\n")
		if msg != "" {
			return fmt.Errorf("pushing branch %s to origin: %s", head.Name().Short(), msg)
		}
		return fmt.Errorf("pushing branch %s to origin: %w", head.Name().Short(), execErr)
	}

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

func (s *Store) ensureStateDir() error {
	if err := os.MkdirAll(filepath.Join(s.repoPath, stateDirName), 0o755); err != nil {
		return fmt.Errorf("creating %s directory: %w", stateDirName, err)
	}
	return nil
}
