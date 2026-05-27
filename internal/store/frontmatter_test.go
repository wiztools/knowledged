package store

import (
	"strings"
	"testing"
	"time"
)

func TestParseFrontmatter(t *testing.T) {
	content := `---
title: "GGUF Models"
description: "Overview of local model files."
tags: [ml, quantization]
created: 2026-04-15T10:22:31Z
modified: 2026-05-25T23:31:02Z
---

# GGUF Models

Body text.
`

	fm, body, err := ParseFrontmatter(content)
	if err != nil {
		t.Fatalf("ParseFrontmatter: %v", err)
	}
	if fm.Title != "GGUF Models" {
		t.Fatalf("Title = %q", fm.Title)
	}
	if fm.Description != "Overview of local model files." {
		t.Fatalf("Description = %q", fm.Description)
	}
	if got, want := strings.Join(fm.Tags, ","), "ml,quantization"; got != want {
		t.Fatalf("Tags = %q, want %q", got, want)
	}
	if got, want := fm.Created.Format(time.RFC3339), "2026-04-15T10:22:31Z"; got != want {
		t.Fatalf("Created = %q, want %q", got, want)
	}
	if !strings.HasPrefix(body, "# GGUF Models") {
		t.Fatalf("body did not preserve Markdown content: %q", body)
	}
}

func TestParseFrontmatter_MissingOpeningFails(t *testing.T) {
	content := "# Plain Note\n\nNo metadata yet.\n"
	if _, _, err := ParseFrontmatter(content); err == nil {
		t.Fatal("expected missing opening delimiter to fail")
	}
}

func TestParseFrontmatter_MissingCloseFails(t *testing.T) {
	_, _, err := ParseFrontmatter("---\ntitle: Missing Close\n")
	if err == nil {
		t.Fatal("expected missing closing delimiter to fail")
	}
}

func TestParseFrontmatter_MissingRequiredFieldFails(t *testing.T) {
	_, _, err := ParseFrontmatter(`---
title: "Missing Description"
tags: []
created: 2026-04-15T10:22:31Z
modified: 2026-05-25T23:31:02Z
---

# Missing Description
`)
	if err == nil {
		t.Fatal("expected missing description to fail")
	}
}

func TestStripFrontmatter_AllowsBodyOnlyInput(t *testing.T) {
	content := "# Plain body\n"
	body, err := StripFrontmatter(content)
	if err != nil {
		t.Fatalf("StripFrontmatter: %v", err)
	}
	if body != content {
		t.Fatalf("body = %q, want %q", body, content)
	}
}

func TestRenderFrontmatterRoundTrip(t *testing.T) {
	created := time.Date(2026, 4, 15, 10, 22, 31, 0, time.UTC)
	modified := time.Date(2026, 5, 25, 23, 31, 2, 0, time.UTC)
	fm := Frontmatter{
		Title:       `A title: with "quotes"`,
		Description: "One-line description.",
		Tags:        []string{"ml", "local-inference"},
		Created:     created,
		Modified:    modified,
	}

	rendered := RenderFrontmatter(fm, "\n# A title\n\nBody.\n")
	if strings.Contains(rendered, "\n\n\n# A title") {
		t.Fatalf("RenderFrontmatter inserted extra leading blank lines:\n%s", rendered)
	}

	gotFM, body, err := ParseFrontmatter(rendered)
	if err != nil {
		t.Fatalf("ParseFrontmatter(rendered): %v", err)
	}
	if gotFM.Title != fm.Title || gotFM.Description != fm.Description {
		t.Fatalf("round-tripped frontmatter mismatch: %#v", gotFM)
	}
	if got, want := strings.Join(gotFM.Tags, ","), "ml,local-inference"; got != want {
		t.Fatalf("Tags = %q, want %q", got, want)
	}
	if gotFM.Created.Format(time.RFC3339) != created.Format(time.RFC3339) {
		t.Fatalf("Created = %s, want %s", gotFM.Created, created)
	}
	if body != "# A title\n\nBody.\n" {
		t.Fatalf("body = %q", body)
	}
}
