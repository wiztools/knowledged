package store

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Frontmatter is the durable per-note metadata stored at the top of each
// Markdown note.
type Frontmatter struct {
	Title       string    `yaml:"title"`
	Description string    `yaml:"description"`
	Tags        []string  `yaml:"tags"`
	Created     time.Time `yaml:"created"`
	Modified    time.Time `yaml:"modified"`
}

// ParseFrontmatter returns the YAML frontmatter and Markdown body from content.
// Stored notes must have frontmatter; missing or incomplete metadata is an
// error after the one-time repository migration.
func ParseFrontmatter(content string) (Frontmatter, string, error) {
	if !hasOpeningFrontmatter(content) {
		return Frontmatter{}, "", fmt.Errorf("frontmatter: missing opening delimiter")
	}

	header, body, ok := splitFrontmatter(content)
	if !ok {
		return Frontmatter{}, "", fmt.Errorf("frontmatter: missing closing delimiter")
	}

	var fm Frontmatter
	if err := yaml.Unmarshal([]byte(header), &fm); err != nil {
		return Frontmatter{}, "", fmt.Errorf("frontmatter: parse yaml: %w", err)
	}
	if fm.Tags == nil {
		fm.Tags = []string{}
	}
	if err := validateFrontmatter(fm); err != nil {
		return Frontmatter{}, "", err
	}
	return fm, body, nil
}

// StripFrontmatter returns the body from content when it is frontmatter-wrapped.
// It leaves ordinary body-only input unchanged; this is for inbound user content,
// not for reading stored notes.
func StripFrontmatter(content string) (string, error) {
	if !hasOpeningFrontmatter(content) {
		return content, nil
	}
	_, body, err := ParseFrontmatter(content)
	if err != nil {
		return "", err
	}
	return body, nil
}

// RenderFrontmatter serializes metadata and body into a Markdown note.
func RenderFrontmatter(fm Frontmatter, body string) string {
	var sb strings.Builder
	sb.WriteString("---\n")
	writeYAMLString(&sb, "title", fm.Title)
	writeYAMLString(&sb, "description", fm.Description)
	writeYAMLStringSlice(&sb, "tags", fm.Tags)
	writeYAMLTime(&sb, "created", fm.Created)
	writeYAMLTime(&sb, "modified", fm.Modified)
	sb.WriteString("---\n\n")
	sb.WriteString(strings.TrimLeft(body, "\r\n"))
	return sb.String()
}

func hasOpeningFrontmatter(content string) bool {
	return content == "---" || strings.HasPrefix(content, "---\n") || strings.HasPrefix(content, "---\r\n")
}

func splitFrontmatter(content string) (header, body string, ok bool) {
	lineEnd := strings.IndexByte(content, '\n')
	if lineEnd < 0 {
		return "", "", false
	}

	bodyStart := lineEnd + 1
	for bodyStart <= len(content) {
		nextEnd := strings.IndexByte(content[bodyStart:], '\n')
		lineEnd := len(content)
		if nextEnd >= 0 {
			lineEnd = bodyStart + nextEnd
		}
		line := strings.TrimSuffix(content[bodyStart:lineEnd], "\r")
		if line == "---" {
			nextBodyStart := lineEnd
			if nextBodyStart < len(content) && content[nextBodyStart] == '\n' {
				nextBodyStart++
			}
			if strings.HasPrefix(content[nextBodyStart:], "\r\n") {
				nextBodyStart += 2
			} else if strings.HasPrefix(content[nextBodyStart:], "\n") {
				nextBodyStart++
			}
			return content[strings.IndexByte(content, '\n')+1 : bodyStart], content[nextBodyStart:], true
		}
		if nextEnd < 0 {
			break
		}
		bodyStart = lineEnd + 1
	}
	return "", "", false
}

func validateFrontmatter(fm Frontmatter) error {
	switch {
	case strings.TrimSpace(fm.Title) == "":
		return fmt.Errorf("frontmatter: missing title")
	case strings.TrimSpace(fm.Description) == "":
		return fmt.Errorf("frontmatter: missing description")
	case fm.Created.IsZero():
		return fmt.Errorf("frontmatter: missing created")
	case fm.Modified.IsZero():
		return fmt.Errorf("frontmatter: missing modified")
	default:
		return nil
	}
}

func writeYAMLString(sb *strings.Builder, key, value string) {
	sb.WriteString(key)
	sb.WriteString(": ")
	sb.WriteString(strconv.Quote(value))
	sb.WriteString("\n")
}

func writeYAMLStringSlice(sb *strings.Builder, key string, values []string) {
	sb.WriteString(key)
	sb.WriteString(": [")
	for i, value := range values {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString(strconv.Quote(value))
	}
	sb.WriteString("]\n")
}

func writeYAMLTime(sb *strings.Builder, key string, value time.Time) {
	sb.WriteString(key)
	sb.WriteString(": ")
	if value.IsZero() {
		sb.WriteString("0001-01-01T00:00:00Z")
	} else {
		sb.WriteString(value.UTC().Format(time.RFC3339))
	}
	sb.WriteString("\n")
}
