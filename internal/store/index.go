package store

import (
	"fmt"
	"sort"
	"strings"
)

const indexFile = "INDEX.md"
const defaultIndexHeader = "# Index\n\n<!-- Auto-managed by knowledged. Do not edit manually. -->\n"

// NoteWithFrontmatter is the indexable metadata for one committed note.
type NoteWithFrontmatter struct {
	Path        string
	Frontmatter Frontmatter
}

// ReadIndex returns the current content of INDEX.md.
func (s *Store) ReadIndex() (string, error) {
	return s.ReadFile(indexFile)
}

// WriteIndex overwrites INDEX.md with the given content and stages it.
func (s *Store) WriteIndex(content string) error {
	return s.WriteFile(indexFile, content)
}

// RebuildAndWriteIndex regenerates INDEX.md as a deterministic projection of
// every note's frontmatter and stages it. This is the ONLY path that writes
// INDEX.md: the index is a pure function of the notes, never hand-spliced, so
// it cannot drift or accumulate duplicate sections.
func (s *Store) RebuildAndWriteIndex() error {
	notes, err := s.ListMarkdownNotes()
	if err != nil {
		return fmt.Errorf("listing notes for index rebuild: %w", err)
	}
	return s.WriteIndex(RebuildIndex(notes))
}

// RebuildIndex renders INDEX.md as a deterministic projection of note
// frontmatter. Notes without a title are skipped because they are not yet
// migratable into a useful index entry.
func RebuildIndex(notes []NoteWithFrontmatter) string {
	return RebuildIndexWithHeader(defaultIndexHeader, notes)
}

// RebuildIndexWithHeader renders INDEX.md with a caller-provided preamble.
func RebuildIndexWithHeader(header string, notes []NoteWithFrontmatter) string {
	type entry struct {
		path        string
		title       string
		description string
	}

	sections := make(map[string][]entry)
	for _, note := range notes {
		path, err := CleanContentPath(note.Path)
		if err != nil || path == indexFile {
			continue
		}
		title := strings.TrimSpace(note.Frontmatter.Title)
		if title == "" {
			continue
		}
		section := sectionForNote(note)
		sections[section] = append(sections[section], entry{
			path:        path,
			title:       title,
			description: strings.TrimSpace(note.Frontmatter.Description),
		})
	}

	names := make([]string, 0, len(sections))
	for name := range sections {
		names = append(names, name)
	}
	sort.Strings(names)

	var sb strings.Builder
	sb.WriteString(strings.TrimRight(header, "\n"))
	sb.WriteString("\n")
	for _, name := range names {
		entries := sections[name]
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].path < entries[j].path
		})
		sb.WriteString("\n## ")
		sb.WriteString(name)
		sb.WriteString("\n")
		for _, entry := range entries {
			sb.WriteString("- [")
			sb.WriteString(entry.title)
			sb.WriteString("](")
			sb.WriteString(entry.path)
			sb.WriteString(")")
			if entry.description != "" {
				sb.WriteString(" — ")
				sb.WriteString(entry.description)
			}
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

// sectionForNote returns the INDEX.md heading a note belongs under. The
// frontmatter Section override wins when set; otherwise the section is derived
// from the note's location. Placement is therefore a pure function of (path,
// optional override) — there is no separately-stored section state to drift.
func sectionForNote(note NoteWithFrontmatter) string {
	if s := strings.TrimSpace(note.Frontmatter.Section); s != "" {
		return s
	}
	return sectionNameForPath(note.Path)
}

// sectionNameForPath derives a section heading from the note's leaf directory
// (the folder immediately containing the file), humanized. A file directly at
// the repo root has no directory and lands in "Notes". Using the leaf folder —
// not the first path segment — lets the directory tree be the taxonomy:
// ai/concepts/llm-architecture/lora.md → "LLM Architecture".
func sectionNameForPath(path string) string {
	slash := strings.LastIndex(path, "/")
	if slash < 0 {
		return "Notes" // top-level file, no directory
	}
	dir := path[:slash]
	if seg := strings.LastIndex(dir, "/"); seg >= 0 {
		dir = dir[seg+1:]
	}
	if dir == "" || dir == "." {
		return "Notes"
	}
	return humanizePathSegment(dir)
}

func humanizePathSegment(segment string) string {
	segment = strings.ReplaceAll(segment, "-", " ")
	segment = strings.ReplaceAll(segment, "_", " ")
	words := strings.Fields(segment)
	if len(words) == 0 {
		return "Notes"
	}
	for i, word := range words {
		words[i] = titleWord(word)
	}
	return strings.Join(words, " ")
}

func titleWord(word string) string {
	upper := strings.ToUpper(word)
	switch upper {
	case "AI", "API", "CLI", "CSS", "HTML", "HTTP", "LLM", "MCP", "ML", "SDK", "SQL", "UI", "UX":
		return upper
	}
	if len(word) == 1 {
		return strings.ToUpper(word)
	}
	return strings.ToUpper(word[:1]) + strings.ToLower(word[1:])
}
