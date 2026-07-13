package store

import (
	"strings"
	"testing"
)

const sampleIndex = `# Index

<!-- Auto-managed by knowledged. Do not edit manually. -->

## Go
- [Goroutines](tech/go/goroutines.md) — concurrency primitives
- [Channels](tech/go/channels.md) — message passing

## Docker
- [Setup](infra/docker/setup.md) — installing docker
`

func TestParseIndex_HeaderAndSections(t *testing.T) {
	p := ParseIndex(sampleIndex)

	if !strings.Contains(p.Header, "# Index") {
		t.Errorf("expected header to contain title, got: %q", p.Header)
	}
	if !strings.Contains(p.Header, "Auto-managed") {
		t.Errorf("expected header to retain comment, got: %q", p.Header)
	}

	if got, want := len(p.Sections), 2; got != want {
		t.Fatalf("expected %d sections, got %d", want, got)
	}
	if p.Sections[0].Name != "Go" {
		t.Errorf("expected first section name 'Go', got %q", p.Sections[0].Name)
	}
	if p.Sections[1].Name != "Docker" {
		t.Errorf("expected second section name 'Docker', got %q", p.Sections[1].Name)
	}
	if !strings.Contains(p.Sections[0].Body, "Goroutines") {
		t.Errorf("expected Go section body to contain Goroutines entry, got: %q", p.Sections[0].Body)
	}
	if !strings.Contains(p.Sections[0].Body, "Channels") {
		t.Errorf("expected Go section body to contain Channels entry, got: %q", p.Sections[0].Body)
	}
}

func TestParseIndex_RoundTrip(t *testing.T) {
	p := ParseIndex(sampleIndex)
	got := p.Render()
	if got != sampleIndex {
		t.Errorf("round-trip mismatch:\n--- want ---\n%s\n--- got ---\n%s", sampleIndex, got)
	}
}

func TestParseIndex_NoSections(t *testing.T) {
	raw := "# Index\n\n<!-- empty -->\n"
	p := ParseIndex(raw)
	if len(p.Sections) != 0 {
		t.Errorf("expected 0 sections, got %d", len(p.Sections))
	}
	if p.Header != raw {
		t.Errorf("expected header to equal input, got: %q", p.Header)
	}
}

func TestSectionNames(t *testing.T) {
	p := ParseIndex(sampleIndex)
	names := p.SectionNames()
	if got, want := strings.Join(names, ","), "Go,Docker"; got != want {
		t.Errorf("expected %q, got %q", want, got)
	}
}

func TestSubtreeFor_PicksOnlyNamed(t *testing.T) {
	p := ParseIndex(sampleIndex)
	sub := p.SubtreeFor([]string{"Go"})

	if !strings.Contains(sub, "## Go") {
		t.Errorf("expected subtree to contain Go heading, got: %q", sub)
	}
	if !strings.Contains(sub, "Goroutines") {
		t.Errorf("expected subtree to contain Go entries, got: %q", sub)
	}
	if strings.Contains(sub, "## Docker") {
		t.Errorf("expected subtree to omit Docker, got: %q", sub)
	}
}

func TestSubtreeFor_CaseInsensitive(t *testing.T) {
	p := ParseIndex(sampleIndex)
	sub := p.SubtreeFor([]string{"go", "  DOCKER  "})

	if !strings.Contains(sub, "## Go") || !strings.Contains(sub, "## Docker") {
		t.Errorf("expected both sections, got: %q", sub)
	}
}

func TestSubtreeFor_UnknownSectionsIgnored(t *testing.T) {
	p := ParseIndex(sampleIndex)
	sub := p.SubtreeFor([]string{"Go", "Frontend"})

	if !strings.Contains(sub, "## Go") {
		t.Errorf("expected Go section in subtree, got: %q", sub)
	}
	if strings.Contains(sub, "Frontend") {
		t.Errorf("expected unknown section to be omitted, got: %q", sub)
	}
}
