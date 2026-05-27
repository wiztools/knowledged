package tagindex

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wiztools/knowledged/internal/store"
)

const (
	indexVersion = 1
	stateFile    = "tag-index.json"
)

// MatchMode controls whether a document must match any requested tag or every
// requested tag.
type MatchMode string

const (
	MatchAny MatchMode = "any"
	MatchAll MatchMode = "all"
)

// TagSummary is the lightweight tag browse response.
type TagSummary struct {
	Tag   string `json:"tag"`
	Count int    `json:"count"`
}

// Document is the metadata cached for one knowledge document.
type Document struct {
	Path        string    `json:"path"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Tags        []string  `json:"tags"`
	Modified    time.Time `json:"modified"`
}

type fileIndex struct {
	Version     int                 `json:"version"`
	GeneratedAt time.Time           `json:"generated_at"`
	GitHead     string              `json:"git_head"`
	Documents   map[string]Document `json:"documents"`
	Tags        map[string][]string `json:"tags"`
}

// TagIndex owns the derived, disposable tag cache under .knowledged/.
type TagIndex struct {
	path  string
	store *store.Store
	mu    sync.Mutex
}

// New creates a TagIndex service backed by .knowledged/tag-index.json.
func New(st *store.Store) *TagIndex {
	return &TagIndex{
		path:  st.StatePath(stateFile),
		store: st,
	}
}

// Ensure creates the cache on first use, or rebuilds it if it is missing,
// malformed, or from an unsupported schema version.
func (ti *TagIndex) Ensure() error {
	ti.mu.Lock()
	defer ti.mu.Unlock()
	_, err := ti.loadLocked()
	if err == nil {
		return nil
	}
	return ti.rebuildLocked()
}

// Rebuild regenerates the full cache from Markdown frontmatter.
func (ti *TagIndex) Rebuild() error {
	ti.mu.Lock()
	defer ti.mu.Unlock()
	return ti.rebuildLocked()
}

// ListTags returns all known tags with document counts.
func (ti *TagIndex) ListTags() ([]TagSummary, error) {
	ti.mu.Lock()
	defer ti.mu.Unlock()

	idx, err := ti.ensureLocked()
	if err != nil {
		return nil, err
	}
	tags := make([]TagSummary, 0, len(idx.Tags))
	for tag, paths := range idx.Tags {
		tags = append(tags, TagSummary{Tag: tag, Count: len(paths)})
	}
	sort.Slice(tags, func(i, j int) bool {
		if tags[i].Tag == tags[j].Tag {
			return tags[i].Count < tags[j].Count
		}
		return tags[i].Tag < tags[j].Tag
	})
	return tags, nil
}

// DocumentsForTags returns document metadata for the requested tags.
func (ti *TagIndex) DocumentsForTags(tags []string, mode MatchMode) ([]Document, error) {
	ti.mu.Lock()
	defer ti.mu.Unlock()

	normalized := normalizeTags(tags)
	if len(normalized) == 0 {
		return nil, fmt.Errorf("at least one tag is required")
	}
	if mode == "" {
		mode = MatchAny
	}
	if mode != MatchAny && mode != MatchAll {
		return nil, fmt.Errorf("match must be %q or %q", MatchAny, MatchAll)
	}

	idx, err := ti.ensureLocked()
	if err != nil {
		return nil, err
	}

	counts := make(map[string]int)
	for _, tag := range normalized {
		for _, path := range idx.Tags[tag] {
			counts[path]++
		}
	}

	docs := make([]Document, 0, len(counts))
	for path, count := range counts {
		if mode == MatchAll && count != len(normalized) {
			continue
		}
		doc, ok := idx.Documents[path]
		if ok {
			docs = append(docs, doc)
		}
	}
	sort.Slice(docs, func(i, j int) bool {
		if docs[i].Modified.Equal(docs[j].Modified) {
			return docs[i].Path < docs[j].Path
		}
		return docs[i].Modified.After(docs[j].Modified)
	})
	return docs, nil
}

// UpsertDocument refreshes the cache entry for a single stored document.
func (ti *TagIndex) UpsertDocument(path string) error {
	ti.mu.Lock()
	defer ti.mu.Unlock()

	idx, err := ti.ensureLocked()
	if err != nil {
		return err
	}
	doc, err := ti.documentForPath(path)
	if err != nil {
		return err
	}
	deleteDocument(idx, doc.Path)
	idx.Documents[doc.Path] = doc
	for _, tag := range doc.Tags {
		idx.Tags[tag] = appendUniqueSorted(idx.Tags[tag], doc.Path)
	}
	idx.GeneratedAt = time.Now().UTC()
	return ti.writeLocked(idx)
}

// RemoveDocument deletes a document from the cache. It is a no-op if absent.
func (ti *TagIndex) RemoveDocument(path string) error {
	ti.mu.Lock()
	defer ti.mu.Unlock()

	idx, err := ti.ensureLocked()
	if err != nil {
		return err
	}
	cleanPath, err := store.CleanContentPath(path)
	if err != nil {
		return err
	}
	deleteDocument(idx, cleanPath)
	idx.GeneratedAt = time.Now().UTC()
	return ti.writeLocked(idx)
}

func (ti *TagIndex) ensureLocked() (*fileIndex, error) {
	idx, err := ti.loadLocked()
	if err == nil {
		return idx, nil
	}
	if err := ti.rebuildLocked(); err != nil {
		return nil, err
	}
	return ti.loadLocked()
}

func (ti *TagIndex) loadLocked() (*fileIndex, error) {
	data, err := os.ReadFile(ti.path)
	if err != nil {
		return nil, err
	}
	var idx fileIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, err
	}
	if idx.Version != indexVersion {
		return nil, fmt.Errorf("unsupported tag index version %d", idx.Version)
	}
	if idx.Documents == nil || idx.Tags == nil {
		return nil, errors.New("tag index is missing required maps")
	}
	head, err := ti.store.HeadHash()
	if err != nil {
		return nil, err
	}
	if idx.GitHead != head {
		return nil, errors.New("tag index is stale")
	}
	return &idx, nil
}

func (ti *TagIndex) rebuildLocked() error {
	notes, err := ti.store.ListMarkdownNotes()
	if err != nil {
		return err
	}
	idx := &fileIndex{
		Version:     indexVersion,
		GeneratedAt: time.Now().UTC(),
		GitHead:     ti.currentHeadLocked(),
		Documents:   make(map[string]Document, len(notes)),
		Tags:        make(map[string][]string),
	}
	for _, note := range notes {
		doc := documentFromNote(note)
		idx.Documents[doc.Path] = doc
		for _, tag := range doc.Tags {
			idx.Tags[tag] = appendUniqueSorted(idx.Tags[tag], doc.Path)
		}
	}
	return ti.writeLocked(idx)
}

func (ti *TagIndex) documentForPath(path string) (Document, error) {
	cleanPath, err := store.CleanContentPath(path)
	if err != nil {
		return Document{}, err
	}
	content, err := ti.store.ReadFile(cleanPath)
	if err != nil {
		return Document{}, err
	}
	fm, _, err := store.ParseFrontmatter(content)
	if err != nil {
		return Document{}, err
	}
	return documentFromNote(store.NoteWithFrontmatter{Path: cleanPath, Frontmatter: fm}), nil
}

func documentFromNote(note store.NoteWithFrontmatter) Document {
	return Document{
		Path:        note.Path,
		Title:       strings.TrimSpace(note.Frontmatter.Title),
		Description: strings.TrimSpace(note.Frontmatter.Description),
		Tags:        normalizeTags(note.Frontmatter.Tags),
		Modified:    note.Frontmatter.Modified.UTC(),
	}
}

func (ti *TagIndex) writeLocked(idx *fileIndex) error {
	idx.GitHead = ti.currentHeadLocked()
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding tag index: %w", err)
	}
	data = append(data, '\n')

	if err := os.MkdirAll(filepath.Dir(ti.path), 0o755); err != nil {
		return fmt.Errorf("creating tag index directory: %w", err)
	}
	tmp := ti.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("opening temp tag index: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("writing temp tag index: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("syncing temp tag index: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing temp tag index: %w", err)
	}
	if err := os.Rename(tmp, ti.path); err != nil {
		return fmt.Errorf("replacing tag index: %w", err)
	}
	return nil
}

func (ti *TagIndex) currentHeadLocked() string {
	head, err := ti.store.HeadHash()
	if err != nil {
		return ""
	}
	return head
}

func deleteDocument(idx *fileIndex, path string) {
	delete(idx.Documents, path)
	for tag, paths := range idx.Tags {
		next := paths[:0]
		for _, p := range paths {
			if p != path {
				next = append(next, p)
			}
		}
		if len(next) == 0 {
			delete(idx.Tags, tag)
		} else {
			idx.Tags[tag] = next
		}
	}
}

func appendUniqueSorted(paths []string, path string) []string {
	for _, existing := range paths {
		if existing == path {
			return paths
		}
	}
	paths = append(paths, path)
	sort.Strings(paths)
	return paths
}

func normalizeTags(tags []string) []string {
	seen := make(map[string]bool, len(tags))
	var out []string
	for _, tag := range tags {
		tag = strings.ToLower(strings.TrimSpace(tag))
		if tag == "" || seen[tag] {
			continue
		}
		seen[tag] = true
		out = append(out, tag)
	}
	sort.Strings(out)
	return out
}
