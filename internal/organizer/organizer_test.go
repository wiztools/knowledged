package organizer

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wiztools/knowledged/internal/llm"
	"github.com/wiztools/knowledged/internal/store"
)

// fakeLLM returns canned replies in order. Tests can record what each
// call looked like (including which schema, if any) via the calls slice.
type fakeLLM struct {
	replies []string
	calls   []fakeCall
}

type fakeCall struct {
	system     string
	user       string
	schema     *llm.Schema // nil for plain Complete; populated for CompleteStructured
	structured bool
}

func (f *fakeLLM) Complete(_ context.Context, system, user string, _ ...llm.CallOption) (string, error) {
	f.calls = append(f.calls, fakeCall{system: system, user: user})
	return f.next()
}

func (f *fakeLLM) CompleteStructured(_ context.Context, system, user string, schema llm.Schema, _ ...llm.CallOption) (string, error) {
	s := schema
	f.calls = append(f.calls, fakeCall{system: system, user: user, schema: &s, structured: true})
	return f.next()
}

func (f *fakeLLM) next() (string, error) {
	if len(f.calls) > len(f.replies) {
		return "", errors.New("fakeLLM: ran out of canned replies")
	}
	return f.replies[len(f.calls)-1], nil
}

func newOrganizerWithIndex(t *testing.T, indexBody string, replies []string) (*Organizer, *fakeLLM, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	st, err := store.New(dir, logger)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	if indexBody != "" {
		if err := st.WriteIndex(indexBody); err != nil {
			t.Fatalf("WriteIndex: %v", err)
		}
		if err := st.Commit("seed"); err != nil {
			t.Fatalf("Commit: %v", err)
		}
	}
	llm := &fakeLLM{replies: replies}
	return New(st, llm, logger), llm, st
}

const seedIndex = `# Index

<!-- Auto-managed by knowledged. Do not edit manually. -->

## Go
- [Goroutines](tech/go/goroutines.md) — concurrency primitives

## Docker
- [Setup](infra/docker/setup.md) — installing docker
`

func TestDecide_TwoPass_RouteThenPlace(t *testing.T) {
	routeReply := `{"candidate_sections":["Go"],"proposed_new_section":""}`
	placeReply := `{
  "target_path": "tech/go/generics.md",
  "title": "Go Generics",
  "description": "Type parameters in Go 1.18+",
  "refactors": [],
  "updated_sections": [
    {"name": "Go", "body": "- [Goroutines](tech/go/goroutines.md) — concurrency primitives\n- [Generics](tech/go/generics.md) — type parameters in Go 1.18+\n"}
  ]
}`

	org, llm, _ := newOrganizerWithIndex(t, seedIndex, []string{routeReply, placeReply})

	d, err := org.Decide(context.Background(), "Go 1.18 added generics. Use [T any] in function signatures.", "go generics", "", nil)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if d.TargetPath != "tech/go/generics.md" {
		t.Errorf("target_path = %q, want %q", d.TargetPath, "tech/go/generics.md")
	}

	if len(llm.calls) != 2 {
		t.Fatalf("expected 2 LLM calls, got %d", len(llm.calls))
	}
	// Pass 1 should NOT contain bullet contents — just headings + counts.
	if strings.Contains(llm.calls[0].user, "Goroutines") {
		t.Errorf("pass 1 prompt leaked bullet contents:\n%s", llm.calls[0].user)
	}
	if !strings.Contains(llm.calls[0].user, "Go (1 entries)") {
		t.Errorf("pass 1 prompt missing heading list line:\n%s", llm.calls[0].user)
	}
	// Pass 2 SHOULD include the Go subtree but NOT the Docker subtree.
	if !strings.Contains(llm.calls[1].user, "Goroutines") {
		t.Errorf("pass 2 prompt missing selected subtree contents:\n%s", llm.calls[1].user)
	}
	if strings.Contains(llm.calls[1].user, "infra/docker") {
		t.Errorf("pass 2 prompt leaked unselected sections:\n%s", llm.calls[1].user)
	}
}

func TestDecide_NewSectionFromTargetFolder(t *testing.T) {
	// The section is implied by the target folder, so a "new section" is just a
	// note placed in a new directory — the model returns only the target path.
	routeReply := `{"candidate_sections":[],"proposed_new_section":"Frontend"}`
	placeReply := `{
  "target_path": "web/frontend/react.md",
  "title": "React",
  "description": "Component-based UI library",
  "refactors": []
}`

	org, _, _ := newOrganizerWithIndex(t, seedIndex, []string{routeReply, placeReply})

	d, err := org.Decide(context.Background(), "React is a component library", "react", "", nil)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if d.TargetPath != "web/frontend/react.md" {
		t.Errorf("target_path = %q, want web/frontend/react.md", d.TargetPath)
	}
}

func TestDecide_UsesSuppliedTitleAndTags(t *testing.T) {
	routeReply := `{"candidate_sections":["Go"],"proposed_new_section":""}`
	placeReply := `{
  "target_path": "tech/go/generics.md",
  "title": "Model Title",
  "description": "Type parameters in Go",
  "tags": ["model-tag"],
  "refactors": [],
  "updated_sections": [
    {"name": "Go", "body": "- [Model Title](tech/go/generics.md) — type parameters in Go\n"}
  ]
}`

	org, llm, _ := newOrganizerWithIndex(t, seedIndex, []string{routeReply, placeReply})

	d, err := org.Decide(context.Background(), "Go 1.18 added generics.", "go generics", "User Title", []string{"go", "language"})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if d.Title != "User Title" {
		t.Fatalf("Title = %q, want supplied title", d.Title)
	}
	if got, want := strings.Join(d.Tags, ","), "go,language"; got != want {
		t.Fatalf("Tags = %q, want %q", got, want)
	}
	if !strings.Contains(llm.calls[1].user, "Title from user: User Title") {
		t.Fatalf("placement prompt missing supplied title:\n%s", llm.calls[1].user)
	}
	if !strings.Contains(llm.calls[1].user, "Tags from user: go, language") {
		t.Fatalf("placement prompt missing supplied tags:\n%s", llm.calls[1].user)
	}
}

func TestDecide_UsesGeneratedTagsWhenNoTagsSupplied(t *testing.T) {
	routeReply := `{"candidate_sections":["Go"],"proposed_new_section":""}`
	placeReply := `{
  "target_path": "tech/go/generics.md",
  "title": "Go Generics",
  "description": "Type parameters in Go",
  "tags": ["go", "generics"],
  "refactors": [],
  "updated_sections": [
    {"name": "Go", "body": "- [Go Generics](tech/go/generics.md) — type parameters in Go\n"}
  ]
}`

	org, _, _ := newOrganizerWithIndex(t, seedIndex, []string{routeReply, placeReply})

	d, err := org.Decide(context.Background(), "Go 1.18 added generics.", "go generics", "", nil)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if d.Title != "Go Generics" {
		t.Fatalf("Title = %q, want generated title", d.Title)
	}
	if got, want := strings.Join(d.Tags, ","), "go,generics"; got != want {
		t.Fatalf("Tags = %q, want %q", got, want)
	}
}

func TestDecide_RejectsEmptyTargetPath(t *testing.T) {
	org, _, _ := newOrganizerWithIndex(t, seedIndex, []string{
		`{"candidate_sections":["Go"],"proposed_new_section":""}`,
		`{"target_path":"","title":"x","description":"x"}`,
	})
	if _, err := org.Decide(context.Background(), "x", "", "", nil); err == nil {
		t.Fatal("expected error for empty target_path, got nil")
	}
}

func TestDecide_UsesStructuredOutput(t *testing.T) {
	routeReply := `{"candidate_sections":["Go"],"proposed_new_section":""}`
	placeReply := `{"target_path":"tech/go/x.md","title":"x","description":"x"}`

	org, llm, _ := newOrganizerWithIndex(t, seedIndex, []string{routeReply, placeReply})
	if _, err := org.Decide(context.Background(), "x", "", "", nil); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if len(llm.calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(llm.calls))
	}
	for i, c := range llm.calls {
		if !c.structured {
			t.Errorf("call %d should have used CompleteStructured", i)
		}
		if c.schema == nil {
			t.Errorf("call %d missing schema", i)
		}
	}
	if llm.calls[0].schema.Name != "route_decision" {
		t.Errorf("call 0 schema = %q, want route_decision", llm.calls[0].schema.Name)
	}
	if llm.calls[1].schema.Name != "placement_decision" {
		t.Errorf("call 1 schema = %q, want placement_decision", llm.calls[1].schema.Name)
	}
}

func TestDecideAvoidingWarnsAboutExistingTargetPath(t *testing.T) {
	routeReply := `{"candidate_sections":["Go"],"proposed_new_section":""}`
	placeReply := `{"target_path":"tech/go/generics-2.md","title":"x","description":"x"}`

	org, llm, _ := newOrganizerWithIndex(t, seedIndex, []string{routeReply, placeReply})
	if _, err := org.DecideAvoiding(context.Background(), "x", "", "", nil, []string{"tech/go/generics.md"}); err != nil {
		t.Fatalf("DecideAvoiding: %v", err)
	}
	if !strings.Contains(llm.calls[0].user, "tech/go/generics.md") {
		t.Fatalf("route prompt missing conflicting path:\n%s", llm.calls[0].user)
	}
	if !strings.Contains(llm.calls[1].user, "MUST NOT be reused or overwritten") {
		t.Fatalf("placement prompt missing overwrite warning:\n%s", llm.calls[1].user)
	}
}

func TestDecide_EmptyIndex(t *testing.T) {
	routeReply := `{"candidate_sections":[],"proposed_new_section":"Go"}`
	placeReply := `{"target_path":"tech/go/intro.md","title":"Go Intro","description":"Intro"}`

	// Use the bootstrapped (empty) index.
	org, llm, _ := newOrganizerWithIndex(t, "", []string{routeReply, placeReply})

	d, err := org.Decide(context.Background(), "Go is a language", "", "", nil)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.TargetPath != "tech/go/intro.md" {
		t.Errorf("target_path = %q, want tech/go/intro.md", d.TargetPath)
	}
	if !strings.Contains(llm.calls[0].user, "(none — INDEX.md has no sections yet)") {
		t.Errorf("pass 1 prompt should mark empty index:\n%s", llm.calls[0].user)
	}
}

func TestExecuteWritesFrontmatter(t *testing.T) {
	org, _, st := newOrganizerWithIndex(t, seedIndex, nil)
	created := time.Date(2026, 5, 27, 10, 0, 0, 0, time.UTC)
	modified := time.Date(2026, 5, 27, 11, 0, 0, 0, time.UTC)
	decision := &Decision{
		TargetPath:  "tech/go/generics.md",
		Title:       "Go Generics",
		Description: "Type parameters in Go",
		Tags:        []string{"go", "language"},
		Created:     created,
		Modified:    modified,
	}

	if err := org.Execute(context.Background(), "job-123", "# Go Generics\n\nBody.\n", decision); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Execute regenerates INDEX.md from the notes on disk; the new note should
	// appear under its leaf-directory section ("Go").
	idx, err := st.ReadIndex()
	if err != nil {
		t.Fatalf("ReadIndex: %v", err)
	}
	if !strings.Contains(idx, "## Go\n- [Go Generics](tech/go/generics.md) — Type parameters in Go\n") {
		t.Fatalf("rebuilt index missing new entry:\n%s", idx)
	}

	content, err := st.ReadFile("tech/go/generics.md")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	fm, body, err := store.ParseFrontmatter(content)
	if err != nil {
		t.Fatalf("ParseFrontmatter: %v", err)
	}
	if fm.Title != decision.Title || fm.Description != decision.Description {
		t.Fatalf("frontmatter mismatch: %#v", fm)
	}
	if got, want := strings.Join(fm.Tags, ","), "go,language"; got != want {
		t.Fatalf("tags = %q, want %q", got, want)
	}
	if fm.Created.Format(time.RFC3339) != created.Format(time.RFC3339) {
		t.Fatalf("created = %s, want %s", fm.Created, created)
	}
	if body != "# Go Generics\n\nBody.\n" {
		t.Fatalf("body = %q", body)
	}
}

func TestExecuteRejectsExistingTargetPath(t *testing.T) {
	org, _, st := newOrganizerWithIndex(t, seedIndex, nil)
	existing := store.RenderFrontmatter(store.Frontmatter{
		Title:       "Go Generics",
		Description: "Existing note",
		Created:     time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC),
		Modified:    time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC),
	}, "original body")
	if err := st.WriteFile("tech/go/generics.md", existing); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := st.Commit("seed existing note"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	decision := &Decision{
		TargetPath:  "tech/go/generics.md",
		Title:       "Replacement",
		Description: "Should not overwrite",
	}
	err := org.Execute(context.Background(), "job-123", "replacement body", decision)
	if !errors.Is(err, store.ErrFileExists) {
		t.Fatalf("expected ErrFileExists, got %v", err)
	}
	got, err := st.ReadFile("tech/go/generics.md")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if got != existing {
		t.Fatalf("existing content was overwritten:\n%s", got)
	}
}
