package store

import "strings"

const indexFile = "INDEX.md"

// ReadIndex returns the current content of INDEX.md.
func (s *Store) ReadIndex() (string, error) {
	return s.ReadFile(indexFile)
}

// WriteIndex overwrites INDEX.md with the given content and stages it.
func (s *Store) WriteIndex(content string) error {
	return s.WriteFile(indexFile, content)
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
