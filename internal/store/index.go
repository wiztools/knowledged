package store

import (
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
		section := sectionNameForPath(path)
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

// RemoveIndexEntry removes the line in INDEX.md that references path and
// stages the result. It is a no-op (not an error) when path is not present.
func (s *Store) RemoveIndexEntry(path string) error {
	cleanPath, err := CleanContentPath(path)
	if err != nil {
		return err
	}
	current, err := s.ReadIndex()
	if err != nil {
		return err
	}

	lines := strings.Split(current, "\n")
	kept := lines[:0]
	for _, line := range lines {
		if strings.Contains(line, "("+cleanPath+")") {
			continue
		}
		kept = append(kept, line)
	}

	// Nothing changed — skip the write.
	if len(kept) == len(lines) {
		return nil
	}

	return s.WriteIndex(strings.Join(kept, "\n"))
}

// UpdateIndexEntry updates the title and/or description for the entry that
// references path. Empty title or description values preserve the existing
// component. Missing index entries are left untouched.
func (s *Store) UpdateIndexEntry(path, title, description string) error {
	cleanPath, err := CleanContentPath(path)
	if err != nil {
		return err
	}
	if strings.TrimSpace(title) == "" && strings.TrimSpace(description) == "" {
		return nil
	}
	current, err := s.ReadIndex()
	if err != nil {
		return err
	}

	lines := strings.Split(current, "\n")
	changed := false
	for i, line := range lines {
		if !strings.Contains(line, "("+cleanPath+")") {
			continue
		}
		existingTitle, existingDescription := parseIndexEntryLine(line, cleanPath)
		nextTitle := strings.TrimSpace(title)
		if nextTitle == "" {
			nextTitle = existingTitle
		}
		nextDescription := strings.TrimSpace(description)
		if nextDescription == "" {
			nextDescription = existingDescription
		}
		next := "- [" + nextTitle + "](" + cleanPath + ")"
		if nextDescription != "" {
			next += " — " + nextDescription
		}
		if line != next {
			lines[i] = next
			changed = true
		}
		break
	}
	if !changed {
		return nil
	}
	return s.WriteIndex(strings.Join(lines, "\n"))
}

func parseIndexEntryLine(line, path string) (string, string) {
	title := strings.TrimSpace(line)
	description := ""
	link := "](" + path + ")"
	if start := strings.Index(title, "["); start >= 0 {
		if end := strings.Index(title[start:], link); end >= 0 {
			rawTitle := title[start+1 : start+end]
			title = strings.TrimSpace(rawTitle)
			rest := strings.TrimSpace(line[start+end+len(link)+1:])
			rest = strings.TrimPrefix(rest, "—")
			description = strings.TrimSpace(rest)
		}
	}
	return title, description
}

func sectionNameForPath(path string) string {
	first := path
	if slash := strings.Index(first, "/"); slash >= 0 {
		first = first[:slash]
	}
	if first == "" || first == "." {
		return "Notes"
	}
	return humanizePathSegment(first)
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
	case "AI", "API", "CLI", "CSS", "HTML", "HTTP", "LLM", "MCP", "SDK", "SQL", "UI", "UX":
		return upper
	}
	if len(word) == 1 {
		return strings.ToUpper(word)
	}
	return strings.ToUpper(word[:1]) + strings.ToLower(word[1:])
}
