package store

const indexFile = "INDEX.md"

// ReadIndex returns the current content of INDEX.md.
func (s *Store) ReadIndex() (string, error) {
	return s.ReadFile(indexFile)
}

// WriteIndex overwrites INDEX.md with the given content and stages it.
func (s *Store) WriteIndex(content string) error {
	return s.WriteFile(indexFile, content)
}
