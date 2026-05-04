package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/wiztools/knowledged/internal/llm"
	"github.com/wiztools/knowledged/internal/queue"
	"github.com/wiztools/knowledged/internal/store"
)

// fakeLLM returns canned replies in order. Tests can inspect what each
// call looked like (including the schema for structured calls) via the
// calls slice.
type fakeLLM struct {
	replies []string
	calls   []fakeCall
}

type fakeCall struct {
	system     string
	user       string
	schema     *llm.Schema
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

func newTestHandler(t *testing.T) (*Handler, *store.Store) {
	t.Helper()
	return newTestHandlerWithLLM(t, nil)
}

func newTestHandlerWithLLM(t *testing.T, llm *fakeLLM) (*Handler, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	st, err := store.New(dir, logger)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	q, err := queue.New(st, nil, nil, logger, 0)
	if err != nil {
		t.Fatalf("queue.New: %v", err)
	}
	if llm == nil {
		return NewHandler(q, st, nil, nil, logger), st
	}
	return NewHandler(q, st, llm, nil, logger), st
}

func TestDeleteContent_Returns202(t *testing.T) {
	h, st := newTestHandler(t)

	// Write a file so there is something to delete.
	if err := st.WriteFile("notes/hello.md", "hello"); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := st.Commit("seed"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"path": "notes/hello.md"})
	req := httptest.NewRequest(http.MethodDelete, "/content", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.DeleteContent(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Errorf("expected 202, got %d — body: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		JobID  string `json:"job_id"`
		Status string `json:"status"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.JobID == "" {
		t.Error("expected non-empty job_id in response")
	}
	if resp.Status != "queued" {
		t.Errorf("expected status %q, got %q", "queued", resp.Status)
	}
}

func TestDeleteContent_EmptyPath(t *testing.T) {
	h, _ := newTestHandler(t)

	body, _ := json.Marshal(map[string]string{"path": "  "})
	req := httptest.NewRequest(http.MethodDelete, "/content", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.DeleteContent(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

const testIndex = `# Index

<!-- Auto-managed by knowledged. Do not edit manually. -->

## Go
- [Goroutines](tech/go/goroutines.md) — concurrency primitives

## Docker
- [Setup](infra/docker/setup.md) — installing docker
`

func seedFile(t *testing.T, st *store.Store, path, body string) {
	t.Helper()
	if err := st.WriteFile(path, body); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}

func seedIndex(t *testing.T, st *store.Store) {
	t.Helper()
	if err := st.WriteIndex(testIndex); err != nil {
		t.Fatalf("WriteIndex: %v", err)
	}
}

func TestSynthesis_TwoPassRelevance_ProseAnswer(t *testing.T) {
	llm := &fakeLLM{replies: []string{
		`{"sections":["Go"]}`, // route
		`{"paths":["tech/go/goroutines.md"],"explanation":""}`, // pick
		`Goroutines are lightweight threads.`,                  // synthesis (prose)
	}}
	h, st := newTestHandlerWithLLM(t, llm)

	seedFile(t, st, "tech/go/goroutines.md", "Goroutines are Go's lightweight threads, scheduled by the runtime.")
	seedIndex(t, st)
	if err := st.Commit("seed"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/content?"+url.Values{"query": {"what are goroutines"}}.Encode(), nil)
	rec := httptest.NewRecorder()
	h.GetContent(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — body: %s", rec.Code, rec.Body.String())
	}
	var resp synthesisResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Answer == "" || !strings.Contains(resp.Answer, "lightweight") {
		t.Errorf("expected prose answer, got: %q", resp.Answer)
	}
	if len(resp.Sources) != 1 || resp.Sources[0] != "tech/go/goroutines.md" {
		t.Errorf("expected one source, got: %v", resp.Sources)
	}

	if len(llm.calls) != 3 {
		t.Fatalf("expected 3 LLM calls (route, pick, synth), got %d", len(llm.calls))
	}

	// Pass 1 (route) shows headings only — no bullet content.
	if strings.Contains(llm.calls[0].user, "tech/go/goroutines.md") {
		t.Errorf("route prompt leaked bullet content:\n%s", llm.calls[0].user)
	}
	if !strings.Contains(llm.calls[0].user, "Go (1 entries)") {
		t.Errorf("route prompt missing heading list:\n%s", llm.calls[0].user)
	}

	// Pass 2 (pick) shows the Go subtree but NOT Docker.
	if !strings.Contains(llm.calls[1].user, "Goroutines") {
		t.Errorf("pick prompt missing Go subtree:\n%s", llm.calls[1].user)
	}
	if strings.Contains(llm.calls[1].user, "infra/docker") {
		t.Errorf("pick prompt leaked Docker subtree:\n%s", llm.calls[1].user)
	}

	// Pass 3 (synthesis) sends a snippet — short doc fits unmodified.
	if !strings.Contains(llm.calls[2].user, "Goroutines are Go's lightweight threads") {
		t.Errorf("synth prompt missing doc snippet:\n%s", llm.calls[2].user)
	}
}

func TestSynthesis_NeedFullEscapeHatch(t *testing.T) {
	// Doc longer than the snippet budget so it gets truncated.
	longDoc := strings.Repeat("filler line about goroutines.\n", 50) + "FINAL_ANSWER_TOKEN at the very end."
	llm := &fakeLLM{replies: []string{
		`{"sections":["Go"]}`,
		`{"paths":["tech/go/goroutines.md"],"explanation":""}`,
		`{"need_full":["tech/go/goroutines.md"]}`,                                // model asks for the full body
		`The final answer is FINAL_ANSWER_TOKEN per the document body provided.`, // pass 4 (full body answer)
	}}
	h, st := newTestHandlerWithLLM(t, llm)

	seedFile(t, st, "tech/go/goroutines.md", longDoc)
	seedIndex(t, st)
	if err := st.Commit("seed"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/content?"+url.Values{"query": {"what's at the end"}}.Encode(), nil)
	rec := httptest.NewRecorder()
	h.GetContent(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — body: %s", rec.Code, rec.Body.String())
	}
	var resp synthesisResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(resp.Answer, "FINAL_ANSWER_TOKEN") {
		t.Errorf("expected full-body answer, got: %q", resp.Answer)
	}

	if len(llm.calls) != 4 {
		t.Fatalf("expected 4 LLM calls (route, pick, snippet-synth, full-synth), got %d", len(llm.calls))
	}

	// Snippet pass should NOT contain the final-answer token (it's past the budget).
	if strings.Contains(llm.calls[2].user, "FINAL_ANSWER_TOKEN") {
		t.Errorf("snippet pass leaked content past the budget:\n%s", llm.calls[2].user)
	}
	if !strings.Contains(llm.calls[2].user, "[truncated") {
		t.Errorf("snippet pass should mark the truncation:\n%s", llm.calls[2].user)
	}

	// Full-body pass MUST include the final token.
	if !strings.Contains(llm.calls[3].user, "FINAL_ANSWER_TOKEN") {
		t.Errorf("full-body pass missing the doc's tail:\n%s", llm.calls[3].user)
	}
}

func TestSynthesis_NoCandidatesFromRoute(t *testing.T) {
	llm := &fakeLLM{replies: []string{
		`{"sections":[]}`,
	}}
	h, st := newTestHandlerWithLLM(t, llm)
	seedIndex(t, st)
	if err := st.Commit("seed"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/content?"+url.Values{"query": {"x"}}.Encode(), nil)
	rec := httptest.NewRecorder()
	h.GetContent(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — body: %s", rec.Code, rec.Body.String())
	}
	var resp synthesisResponse
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	if !strings.Contains(resp.Answer, "No relevant documents") {
		t.Errorf("expected no-results message, got: %q", resp.Answer)
	}
	if len(llm.calls) != 1 {
		t.Errorf("expected only the route call, got %d", len(llm.calls))
	}
}

func TestSynthesis_RawMode_StopsAfterPick(t *testing.T) {
	llm := &fakeLLM{replies: []string{
		`{"sections":["Go"]}`,
		`{"paths":["tech/go/goroutines.md"],"explanation":""}`,
	}}
	h, st := newTestHandlerWithLLM(t, llm)
	seedFile(t, st, "tech/go/goroutines.md", "body")
	seedIndex(t, st)
	if err := st.Commit("seed"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet,
		"/content?"+url.Values{"query": {"goroutines"}, "mode": {"raw"}}.Encode(), nil)
	rec := httptest.NewRecorder()
	h.GetContent(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — body: %s", rec.Code, rec.Body.String())
	}
	var docs []rawDocResponse
	if err := json.NewDecoder(rec.Body).Decode(&docs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(docs) != 1 || docs[0].Path != "tech/go/goroutines.md" {
		t.Errorf("unexpected docs: %+v", docs)
	}
	if len(llm.calls) != 2 {
		t.Errorf("raw mode should make 2 LLM calls (route+pick), got %d", len(llm.calls))
	}
}

func TestSnippet_TruncatesPastBudget(t *testing.T) {
	short := "exactly five"
	if got := snippet(short, 100); got != short {
		t.Errorf("short content should pass through, got: %q", got)
	}

	long := strings.Repeat("a", 1000)
	got := snippet(long, 100)
	if !strings.Contains(got, "[truncated") {
		t.Errorf("long content should be marked truncated, got: %q", got)
	}
	if len(got) > 200 {
		t.Errorf("truncated snippet way too long: %d chars", len(got))
	}
}

func TestParseNeedFull_DetectsJSONShape(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		wantPaths []string
		wantOK    bool
	}{
		{"prose", "Goroutines are threads.", nil, false},
		{"plain JSON", `{"need_full":["a.md","b.md"]}`, []string{"a.md", "b.md"}, true},
		{"fenced JSON", "```json\n{\"need_full\":[\"a.md\"]}\n```", []string{"a.md"}, true},
		{"empty list", `{"need_full":[]}`, nil, false},
		{"different key", `{"answer":"hello"}`, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			paths, ok := parseNeedFull(tc.input)
			if ok != tc.wantOK {
				t.Errorf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !equalStringSlices(paths, tc.wantPaths) {
				t.Errorf("paths = %v, want %v", paths, tc.wantPaths)
			}
		})
	}
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
