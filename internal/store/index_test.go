package store

import (
	"strings"
	"testing"
)

func TestRebuildIndex(t *testing.T) {
	got := RebuildIndex([]NoteWithFrontmatter{
		{
			Path: "ai/concepts/llm-architecture/lora.md",
			Frontmatter: Frontmatter{
				Title:       "LoRA",
				Description: "Low-rank adaptation",
			},
		},
		{
			Path: "notes/root.md",
			Frontmatter: Frontmatter{
				Title: "Root Note",
			},
		},
		{
			Path: "ai/concepts/llm-architecture/speculative-decoding.md",
			Frontmatter: Frontmatter{
				Title:       "Speculative Decoding",
				Description: "Draft + target model",
			},
		},
	})

	// Sections are the leaf directory, humanized, sorted alphabetically.
	want := `# Index

<!-- Auto-managed by knowledged. Do not edit manually. -->

## LLM Architecture
- [LoRA](ai/concepts/llm-architecture/lora.md) — Low-rank adaptation
- [Speculative Decoding](ai/concepts/llm-architecture/speculative-decoding.md) — Draft + target model

## Notes
- [Root Note](notes/root.md)
`
	if got != want {
		t.Fatalf("RebuildIndex mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestRebuildIndex_SkipsInvalidAndUntitledNotes(t *testing.T) {
	got := RebuildIndex([]NoteWithFrontmatter{
		{Path: "INDEX.md", Frontmatter: Frontmatter{Title: "Index"}},
		{Path: ".knowledged/state.md", Frontmatter: Frontmatter{Title: "State"}},
		{Path: "notes/untitled.md", Frontmatter: Frontmatter{}},
		{Path: "source-materials/web.md", Frontmatter: Frontmatter{Title: "Web Sources"}},
	})

	if strings.Contains(got, "](INDEX.md)") || strings.Contains(got, "](.knowledged/state.md)") || strings.Contains(got, "untitled") {
		t.Fatalf("RebuildIndex included skipped notes:\n%s", got)
	}
	if !strings.Contains(got, "## Source Materials\n- [Web Sources](source-materials/web.md)\n") {
		t.Fatalf("RebuildIndex did not render humanized section:\n%s", got)
	}
}

func TestRebuildIndex_SectionOverride(t *testing.T) {
	got := RebuildIndex([]NoteWithFrontmatter{
		{
			Path: "videos/ai-recommendations/clarity.md",
			Frontmatter: Frontmatter{
				Title:       "Clarity",
				Description: "New skill in the age of AI",
				Section:     "AI & The Future of Work",
			},
		},
	})

	// The frontmatter override wins over the path-derived "Ai Recommendations".
	if !strings.Contains(got, "## AI & The Future of Work\n- [Clarity](videos/ai-recommendations/clarity.md) — New skill in the age of AI\n") {
		t.Fatalf("override not honored:\n%s", got)
	}
	if strings.Contains(got, "## Ai Recommendations") {
		t.Fatalf("path-derived section leaked despite override:\n%s", got)
	}
}

func TestSectionNameForPath(t *testing.T) {
	cases := map[string]string{
		"ai/concepts/llm-architecture/lora.md": "LLM Architecture", // leaf dir, acronym upper-cased
		"ai/concepts/mixture-of-experts.md":    "Concepts",         // parent dir humanized
		"notes/ml/gguf.md":                     "ML",
		"docs/harness-engineering.md":          "Docs",
		"root.md":                              "Notes", // top-level file, no directory
	}
	for path, want := range cases {
		if got := sectionNameForPath(path); got != want {
			t.Errorf("sectionNameForPath(%q) = %q, want %q", path, got, want)
		}
	}
}
