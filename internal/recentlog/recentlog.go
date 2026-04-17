package recentlog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

const compactThreshold = 512
const keepCount = 20

// Entry is one line in the JSONL append log.
type Entry struct {
	JobID     string    `json:"job_id"`
	Path      string    `json:"path"`
	Tags      []string  `json:"tags,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// RecentLog is a JSONL append log stored at a single file path.
// It compacts itself down to keepCount entries once it reaches compactThreshold lines.
type RecentLog struct {
	path   string
	mu     sync.Mutex
	logger *slog.Logger
}

// New creates a RecentLog backed by the given file path.
func New(path string, logger *slog.Logger) *RecentLog {
	return &RecentLog{path: path, logger: logger}
}

// Append adds a new entry to the log and compacts if the line count hits the threshold.
func (r *RecentLog) Append(entry Entry) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	b, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("recentlog marshal: %w", err)
	}

	f, err := os.OpenFile(r.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("recentlog open: %w", err)
	}
	_, writeErr := fmt.Fprintf(f, "%s\n", b)
	closeErr := f.Close()
	if writeErr != nil {
		return fmt.Errorf("recentlog write: %w", writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("recentlog close: %w", closeErr)
	}

	n, err := r.countLines()
	if err != nil {
		r.logger.Warn("recentlog: failed to count lines after append", "error", err)
		return nil
	}
	if n >= compactThreshold {
		if err := r.compact(); err != nil {
			r.logger.Warn("recentlog: compaction failed", "error", err)
		}
	}
	return nil
}

// Recent returns up to n entries in descending order (newest first).
func (r *RecentLog) Recent(n int) ([]Entry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	entries, err := r.readAll()
	if err != nil {
		return nil, err
	}

	// Reverse in-place so newest is first.
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
	if len(entries) > n {
		entries = entries[:n]
	}
	return entries, nil
}

// compact rewrites the file keeping only the newest keepCount entries.
func (r *RecentLog) compact() error {
	entries, err := r.readAll()
	if err != nil {
		return err
	}
	if len(entries) <= keepCount {
		return nil
	}
	keep := entries[len(entries)-keepCount:]
	r.logger.Info("recentlog: compacting", "before", len(entries), "after", len(keep))
	return r.writeAll(keep)
}

func (r *RecentLog) countLines() (int, error) {
	f, err := os.Open(r.path)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		if sc.Text() != "" {
			n++
		}
	}
	return n, sc.Err()
}

func (r *RecentLog) readAll() ([]Entry, error) {
	f, err := os.Open(r.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("recentlog read: %w", err)
	}
	defer f.Close()

	var entries []Entry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		var e Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			r.logger.Warn("recentlog: skipping malformed line", "error", err)
			continue
		}
		entries = append(entries, e)
	}
	return entries, sc.Err()
}

func (r *RecentLog) writeAll(entries []Entry) error {
	tmp := r.path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("recentlog compact create tmp: %w", err)
	}
	w := bufio.NewWriter(f)
	for _, e := range entries {
		b, err := json.Marshal(e)
		if err != nil {
			f.Close()
			os.Remove(tmp)
			return fmt.Errorf("recentlog compact marshal: %w", err)
		}
		fmt.Fprintf(w, "%s\n", b)
	}
	if err := w.Flush(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("recentlog compact flush: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("recentlog compact close: %w", err)
	}
	return os.Rename(tmp, r.path)
}
