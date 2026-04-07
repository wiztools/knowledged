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
	current, err := s.ReadIndex()
	if err != nil {
		return err
	}

	lines := strings.Split(current, "\n")
	kept := lines[:0]
	for _, line := range lines {
		if strings.Contains(line, "("+path+")") {
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
