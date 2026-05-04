package organizer

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"

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

func (f *fakeLLM) Complete(_ context.Context, system, user string) (string, error) {
	f.calls = append(f.calls, fakeCall{system: system, user: user})
	return f.next()
}

func (f *fakeLLM) CompleteStructured(_ context.Context, system, user string, schema llm.Schema) (string, error) {
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

	d, err := org.Decide(context.Background(), "Go 1.18 added generics. Use [T any] in function signatures.", "go generics", nil)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if d.TargetPath != "tech/go/generics.md" {
		t.Errorf("target_path = %q, want %q", d.TargetPath, "tech/go/generics.md")
	}
	if !strings.Contains(d.UpdatedIndex, "Generics") {
		t.Errorf("updated index missing new entry:\n%s", d.UpdatedIndex)
	}
	if !strings.Contains(d.UpdatedIndex, "## Docker") {
		t.Errorf("updated index lost Docker section (splice failed):\n%s", d.UpdatedIndex)
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

func TestDecide_NewSectionAppended(t *testing.T) {
	routeReply := `{"candidate_sections":[],"proposed_new_section":"Frontend"}`
	placeReply := `{
  "target_path": "web/frontend/react.md",
  "title": "React",
  "description": "Component-based UI library",
  "refactors": [],
  "updated_sections": [
    {"name": "Frontend", "body": "- [React](web/frontend/react.md) — component-based UI library\n"}
  ]
}`

	org, _, _ := newOrganizerWithIndex(t, seedIndex, []string{routeReply, placeReply})

	d, err := org.Decide(context.Background(), "React is a component library", "react", nil)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if !strings.Contains(d.UpdatedIndex, "## Frontend") {
		t.Errorf("expected new section appended:\n%s", d.UpdatedIndex)
	}
	if !strings.Contains(d.UpdatedIndex, "## Go") || !strings.Contains(d.UpdatedIndex, "## Docker") {
		t.Errorf("expected existing sections preserved:\n%s", d.UpdatedIndex)
	}
}

func TestDecide_RejectsEmptyTargetPath(t *testing.T) {
	org, _, _ := newOrganizerWithIndex(t, seedIndex, []string{
		`{"candidate_sections":["Go"],"proposed_new_section":""}`,
		`{"target_path":"","title":"x","description":"x","updated_sections":[{"name":"Go","body":"-"}]}`,
	})
	if _, err := org.Decide(context.Background(), "x", "", nil); err == nil {
		t.Fatal("expected error for empty target_path, got nil")
	}
}

func TestDecide_RejectsMissingUpdatedSections(t *testing.T) {
	org, _, _ := newOrganizerWithIndex(t, seedIndex, []string{
		`{"candidate_sections":["Go"],"proposed_new_section":""}`,
		`{"target_path":"tech/go/x.md","title":"x","description":"x","updated_sections":[]}`,
	})
	if _, err := org.Decide(context.Background(), "x", "", nil); err == nil {
		t.Fatal("expected error for empty updated_sections, got nil")
	}
}

func TestDecide_UsesStructuredOutput(t *testing.T) {
	routeReply := `{"candidate_sections":["Go"],"proposed_new_section":""}`
	placeReply := `{"target_path":"tech/go/x.md","title":"x","description":"x","updated_sections":[{"name":"Go","body":"- [x](tech/go/x.md) — y\n"}]}`

	org, llm, _ := newOrganizerWithIndex(t, seedIndex, []string{routeReply, placeReply})
	if _, err := org.Decide(context.Background(), "x", "", nil); err != nil {
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

func TestDecide_EmptyIndex(t *testing.T) {
	routeReply := `{"candidate_sections":[],"proposed_new_section":"Go"}`
	placeReply := `{"target_path":"tech/go/intro.md","title":"Go Intro","description":"Intro","updated_sections":[{"name":"Go","body":"- [Go Intro](tech/go/intro.md) — intro\n"}]}`

	// Use the bootstrapped (empty) index.
	org, llm, _ := newOrganizerWithIndex(t, "", []string{routeReply, placeReply})

	d, err := org.Decide(context.Background(), "Go is a language", "", nil)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if !strings.Contains(d.UpdatedIndex, "## Go") {
		t.Errorf("expected new Go section in updated index:\n%s", d.UpdatedIndex)
	}
	if !strings.Contains(llm.calls[0].user, "(none — INDEX.md has no sections yet)") {
		t.Errorf("pass 1 prompt should mark empty index:\n%s", llm.calls[0].user)
	}
}
