package store

import "strings"

// IndexSection is one `## Heading` block in INDEX.md and the lines that
// follow it (until the next heading or EOF). Body is preserved verbatim
// including any trailing blank lines.
type IndexSection struct {
	Name string
	Body string
}

// ParsedIndex is INDEX.md split into its preamble and ordered sections.
type ParsedIndex struct {
	// Header is everything before the first `## ` heading — title, comments,
	// blank lines. Preserved verbatim on Render.
	Header string
	// Sections are the `## Heading` blocks in source order.
	Sections []IndexSection
}

// ParseIndex splits raw INDEX.md content into header + sections by scanning
// for `## ` headings at the start of a line. Anything before the first
// heading is Header. The parser slices the original string directly so that
// ParsedIndex{}.Render() round-trips byte-for-byte for well-formed inputs.
func ParseIndex(raw string) ParsedIndex {
	type boundary struct {
		lineStart int // byte offset of the `## ` line
		nameStart int // byte offset after `## `
		nameEnd   int // byte offset of the trailing `\n` (or len(raw))
	}
	var bounds []boundary

	lineStart := 0
	for i := 0; i <= len(raw); i++ {
		atEOF := i == len(raw)
		if !atEOF && raw[i] != '\n' {
			continue
		}
		line := raw[lineStart:i]
		if strings.HasPrefix(line, "## ") {
			bounds = append(bounds, boundary{
				lineStart: lineStart,
				nameStart: lineStart + len("## "),
				nameEnd:   i,
			})
		}
		lineStart = i + 1
	}

	if len(bounds) == 0 {
		return ParsedIndex{Header: raw}
	}

	header := raw[:bounds[0].lineStart]
	sections := make([]IndexSection, len(bounds))
	for i, b := range bounds {
		bodyStart := b.nameEnd
		if bodyStart < len(raw) && raw[bodyStart] == '\n' {
			bodyStart++
		}
		bodyEnd := len(raw)
		if i+1 < len(bounds) {
			bodyEnd = bounds[i+1].lineStart
		}
		sections[i] = IndexSection{
			Name: raw[b.nameStart:b.nameEnd],
			Body: raw[bodyStart:bodyEnd],
		}
	}
	return ParsedIndex{Header: header, Sections: sections}
}

// Render serialises the parsed index back to a string. Round-trippable for
// well-formed inputs; appends a trailing newline to bodies that don't already
// end in one (so spliced/replaced sections don't run into the next heading).
func (p ParsedIndex) Render() string {
	var sb strings.Builder
	sb.WriteString(p.Header)
	for _, s := range p.Sections {
		sb.WriteString("## ")
		sb.WriteString(s.Name)
		sb.WriteString("\n")
		sb.WriteString(s.Body)
		if !strings.HasSuffix(s.Body, "\n") {
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

// SectionNames returns the headings in source order, trimmed.
func (p ParsedIndex) SectionNames() []string {
	names := make([]string, len(p.Sections))
	for i, s := range p.Sections {
		names[i] = strings.TrimSpace(s.Name)
	}
	return names
}

// SubtreeFor returns the header plus only the named sections, rendered.
// Matching is case-insensitive on trimmed names. Sections not found are
// silently omitted — the LLM may hallucinate names and we should not crash.
func (p ParsedIndex) SubtreeFor(names []string) string {
	wanted := make(map[string]bool, len(names))
	for _, n := range names {
		wanted[normaliseSectionName(n)] = true
	}
	var subset []IndexSection
	for _, s := range p.Sections {
		if wanted[normaliseSectionName(s.Name)] {
			subset = append(subset, s)
		}
	}
	return ParsedIndex{Header: p.Header, Sections: subset}.Render()
}

// ReplaceSections returns a copy where any section whose name matches an
// entry in updates is replaced. Updates whose names match no existing
// section are appended in input order. Matching is case-insensitive on
// trimmed names; the original heading casing is preserved on update.
func (p ParsedIndex) ReplaceSections(updates []IndexSection) ParsedIndex {
	byName := make(map[string]string, len(updates))
	for _, u := range updates {
		byName[normaliseSectionName(u.Name)] = u.Body
	}
	seen := make(map[string]bool, len(updates))
	out := make([]IndexSection, 0, len(p.Sections)+len(updates))
	for _, s := range p.Sections {
		key := normaliseSectionName(s.Name)
		if body, ok := byName[key]; ok {
			out = append(out, IndexSection{Name: s.Name, Body: body})
			seen[key] = true
			continue
		}
		out = append(out, s)
	}
	for _, u := range updates {
		key := normaliseSectionName(u.Name)
		if !seen[key] {
			out = append(out, IndexSection{Name: strings.TrimSpace(u.Name), Body: u.Body})
		}
	}
	return ParsedIndex{Header: p.Header, Sections: out}
}

func normaliseSectionName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}
