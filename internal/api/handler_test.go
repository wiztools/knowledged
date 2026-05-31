package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wiztools/knowledged/internal/llm"
	"github.com/wiztools/knowledged/internal/queue"
	"github.com/wiztools/knowledged/internal/recentlog"
	"github.com/wiztools/knowledged/internal/store"
	"github.com/wiztools/knowledged/internal/tagindex"
)

// fakeLLM returns canned replies in order. Tests can inspect what each
// call looked like (including the schema for structured calls) via the
// calls slice.
type fakeLLM struct {
	replies []string
	calls   []fakeCall
}

type fakeCall struct {
	system          string
	user            string
	schema          *llm.Schema
	structured      bool
	reasoningBudget int
}

func (f *fakeLLM) Complete(_ context.Context, system, user string, opts ...llm.CallOption) (string, error) {
	f.calls = append(f.calls, fakeCall{
		system:          system,
		user:            user,
		reasoningBudget: llm.ResolveReasoningBudget(opts),
	})
	return f.next()
}

func (f *fakeLLM) CompleteStructured(_ context.Context, system, user string, schema llm.Schema, opts ...llm.CallOption) (string, error) {
	s := schema
	f.calls = append(f.calls, fakeCall{
		system:          system,
		user:            user,
		schema:          &s,
		structured:      true,
		reasoningBudget: llm.ResolveReasoningBudget(opts),
	})
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
	q, err := queue.New(st, nil, nil, nil, logger, 0)
	if err != nil {
		t.Fatalf("queue.New: %v", err)
	}
	if llm == nil {
		return NewHandler(q, st, nil, nil, nil, logger, 0), st
	}
	return NewHandler(q, st, llm, nil, nil, logger, 0), st
}

// newTestHandlerWithBudget is like newTestHandlerWithLLM but lets the test
// assert that /ask forwards the configured reasoning budget as an option.
func newTestHandlerWithBudget(t *testing.T, fl *fakeLLM, budget int) (*Handler, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	st, err := store.New(dir, logger)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	q, err := queue.New(st, nil, nil, nil, logger, 0)
	if err != nil {
		t.Fatalf("queue.New: %v", err)
	}
	return NewHandler(q, st, fl, nil, nil, logger, budget), st
}

func newTestHandlerWithTags(t *testing.T) (*Handler, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	st, err := store.New(dir, logger)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ti := tagindex.New(st)
	q, err := queue.New(st, nil, nil, ti, logger, 0)
	if err != nil {
		t.Fatalf("queue.New: %v", err)
	}
	return NewHandler(q, st, nil, nil, ti, logger, 0), st
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

func TestGetRecentPosts_UsesRequestedLimit(t *testing.T) {
	h, st := newTestHandler(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	rl := recentlog.New(st.StatePath("recent.jsonl"), logger)
	h.recentLog = rl

	for i := 1; i <= 35; i++ {
		path := fmt.Sprintf("notes/post-%02d.md", i)
		ts := time.Date(2026, 5, 31, 12, i, 0, 0, time.UTC)
		content := store.RenderFrontmatter(store.Frontmatter{
			Title:       fmt.Sprintf("Post %02d", i),
			Description: "Test post",
			Tags:        []string{fmt.Sprintf("tag-%02d", i)},
			Created:     ts,
			Modified:    ts,
		}, fmt.Sprintf("# Post %02d\n", i))
		if err := st.WriteFile(path, content); err != nil {
			t.Fatalf("WriteFile %s: %v", path, err)
		}
		if err := rl.Append(recentlog.Entry{
			JobID:     fmt.Sprintf("job-%02d", i),
			Path:      path,
			CreatedAt: ts,
		}); err != nil {
			t.Fatalf("Append %s: %v", path, err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/posts/recents?limit=32", nil)
	rec := httptest.NewRecorder()

	h.GetRecentPosts(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp recentPostsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Posts) != 32 {
		t.Fatalf("len(posts) = %d, want 32", len(resp.Posts))
	}
	if got, want := resp.Posts[0].JobID, "job-35"; got != want {
		t.Fatalf("first job = %s, want %s", got, want)
	}
	if got, want := resp.Posts[31].JobID, "job-04"; got != want {
		t.Fatalf("last job = %s, want %s", got, want)
	}
	if got, want := resp.Posts[0].Tags, []string{"tag-35"}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("first tags = %v, want %v", got, want)
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

func TestPutContent_Returns202(t *testing.T) {
	h, st := newTestHandler(t)

	existing := store.RenderFrontmatter(store.Frontmatter{
		Title:       "Hello",
		Description: "Existing note",
		Tags:        []string{},
		Created:     time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC),
		Modified:    time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC),
	}, "old")
	if err := st.WriteFile("notes/hello.md", existing); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := st.Commit("seed"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	body, _ := json.Marshal(map[string]string{
		"path":    "notes/hello.md",
		"content": "new",
	})
	req := httptest.NewRequest(http.MethodPut, "/content", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.PutContent(rec, req)

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

func TestPutContent_RejectsInvalidInput(t *testing.T) {
	h, _ := newTestHandler(t)

	cases := []map[string]string{
		{"path": "notes/hello.md", "content": "  "},
		{"path": "  ", "content": "new"},
		{"path": "../outside.md", "content": "new"},
	}
	for _, bodyMap := range cases {
		body, _ := json.Marshal(bodyMap)
		req := httptest.NewRequest(http.MethodPut, "/content", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()

		h.PutContent(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for %#v, got %d — body: %s", bodyMap, rec.Code, rec.Body.String())
		}
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

func seedTaggedNote(t *testing.T, st *store.Store, path, title string, tags []string, modified time.Time) {
	t.Helper()
	content := store.RenderFrontmatter(store.Frontmatter{
		Title:       title,
		Description: title + " description",
		Tags:        tags,
		Created:     modified.Add(-time.Hour),
		Modified:    modified,
	}, "body")
	seedFile(t, st, path, content)
}

func TestGetTags_RebuildsAndReturnsCounts(t *testing.T) {
	h, st := newTestHandlerWithTags(t)
	seedTaggedNote(t, st, "go/a.md", "A", []string{"go", "api"}, time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC))
	seedTaggedNote(t, st, "go/b.md", "B", []string{"go"}, time.Date(2026, 5, 2, 10, 0, 0, 0, time.UTC))

	req := httptest.NewRequest(http.MethodGet, "/tags", nil)
	rec := httptest.NewRecorder()
	h.GetTags(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — body: %s", rec.Code, rec.Body.String())
	}
	var resp tagsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Tags) != 2 {
		t.Fatalf("expected 2 tags, got %+v", resp.Tags)
	}
	if resp.Tags[1].Tag != "go" || resp.Tags[1].Count != 2 {
		t.Fatalf("expected go count 2, got %+v", resp.Tags)
	}
}

func TestSearch_ByTagReturnsMetadataOrRawDocs(t *testing.T) {
	h, st := newTestHandlerWithTags(t)
	seedTaggedNote(t, st, "go/a.md", "A", []string{"go", "api"}, time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC))
	seedTaggedNote(t, st, "go/b.md", "B", []string{"go"}, time.Date(2026, 5, 2, 10, 0, 0, 0, time.UTC))

	req := httptest.NewRequest(http.MethodGet,
		"/search?"+url.Values{"tags": {"go,api"}, "match": {"all"}}.Encode(), nil)
	rec := httptest.NewRecorder()
	h.Search(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — body: %s", rec.Code, rec.Body.String())
	}
	var docs []tagindex.Document
	if err := json.NewDecoder(rec.Body).Decode(&docs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(docs) != 1 || docs[0].Path != "go/a.md" || docs[0].Title != "A" {
		t.Fatalf("unexpected tagged metadata: %+v", docs)
	}

	rawReq := httptest.NewRequest(http.MethodGet,
		"/search?"+url.Values{"tag": {"api"}, "mode": {"raw"}}.Encode(), nil)
	rawRec := httptest.NewRecorder()
	h.Search(rawRec, rawReq)
	if rawRec.Code != http.StatusOK {
		t.Fatalf("expected raw 200, got %d — body: %s", rawRec.Code, rawRec.Body.String())
	}
	var rawDocs []rawDocResponse
	if err := json.NewDecoder(rawRec.Body).Decode(&rawDocs); err != nil {
		t.Fatalf("raw decode: %v", err)
	}
	if len(rawDocs) != 1 || rawDocs[0].Path != "go/a.md" || !strings.Contains(rawDocs[0].Content, "tags:") {
		t.Fatalf("unexpected raw docs: %+v", rawDocs)
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

	req := httptest.NewRequest(http.MethodGet, "/answer?"+url.Values{"query": {"what are goroutines"}}.Encode(), nil)
	rec := httptest.NewRecorder()
	h.Answer(rec, req)

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

	req := httptest.NewRequest(http.MethodGet, "/answer?"+url.Values{"query": {"what's at the end"}}.Encode(), nil)
	rec := httptest.NewRecorder()
	h.Answer(rec, req)

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

	req := httptest.NewRequest(http.MethodGet, "/answer?"+url.Values{"query": {"x"}}.Encode(), nil)
	rec := httptest.NewRecorder()
	h.Answer(rec, req)

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

func TestSearch_QueryRawMode_StopsAfterPick(t *testing.T) {
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
		"/search?"+url.Values{"query": {"goroutines"}, "mode": {"raw"}}.Encode(), nil)
	rec := httptest.NewRecorder()
	h.Search(rec, req)

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

func TestPostAsk_ReturnsAnswerAndTags(t *testing.T) {
	fake := &fakeLLM{replies: []string{
		`{"answer":"## Goroutines\n\nLightweight threads managed by the Go runtime.\n","tags":["golang","concurrency"," "]}`,
	}}
	h, _ := newTestHandlerWithLLM(t, fake)

	body, _ := json.Marshal(map[string]string{"question": "what are goroutines?"})
	req := httptest.NewRequest(http.MethodPost, "/ask", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.PostAsk(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — body: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Question string   `json:"question"`
		Answer   string   `json:"answer"`
		Tags     []string `json:"tags"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Question != "what are goroutines?" {
		t.Errorf("question = %q, want %q", resp.Question, "what are goroutines?")
	}
	if !strings.HasPrefix(resp.Answer, "## Goroutines") {
		t.Errorf("answer should preserve Markdown, got %q", resp.Answer)
	}
	if strings.HasSuffix(resp.Answer, "\n") {
		t.Errorf("answer should be trimmed of trailing whitespace, got %q", resp.Answer)
	}
	want := []string{"golang", "concurrency"}
	if !equalStringSlices(resp.Tags, want) {
		t.Errorf("tags = %v, want %v (whitespace-only entries should be dropped)", resp.Tags, want)
	}

	if len(fake.calls) != 1 {
		t.Fatalf("expected 1 LLM call, got %d", len(fake.calls))
	}
	if !fake.calls[0].structured {
		t.Error("ask should use CompleteStructured so tags + answer come back together")
	}
	if fake.calls[0].schema == nil || fake.calls[0].schema.Name != "knowledge_draft" {
		t.Errorf("expected schema named knowledge_draft, got %+v", fake.calls[0].schema)
	}
	if fake.calls[0].user != "what are goroutines?" {
		t.Errorf("user prompt = %q, want raw question", fake.calls[0].user)
	}
	if !strings.Contains(fake.calls[0].system, "tags") {
		t.Error("system prompt should mention tags")
	}
}

func TestPostAsk_ForwardsReasoningBudget(t *testing.T) {
	fake := &fakeLLM{replies: []string{`{"answer":"## X\n\nstub","tags":[]}`}}
	h, _ := newTestHandlerWithBudget(t, fake, 2048)

	body, _ := json.Marshal(map[string]string{"question": "what is X?"})
	req := httptest.NewRequest(http.MethodPost, "/ask", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.PostAsk(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — body: %s", rec.Code, rec.Body.String())
	}
	if len(fake.calls) != 1 {
		t.Fatalf("expected 1 LLM call, got %d", len(fake.calls))
	}
	if fake.calls[0].reasoningBudget != 2048 {
		t.Errorf("reasoning budget = %d, want %d", fake.calls[0].reasoningBudget, 2048)
	}
}

func TestPostAsk_ZeroBudgetSkipsReasoning(t *testing.T) {
	fake := &fakeLLM{replies: []string{`{"answer":"## X\n\nstub","tags":[]}`}}
	h, _ := newTestHandlerWithBudget(t, fake, 0)

	body, _ := json.Marshal(map[string]string{"question": "what is X?"})
	req := httptest.NewRequest(http.MethodPost, "/ask", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.PostAsk(rec, req)

	if fake.calls[0].reasoningBudget != 0 {
		t.Errorf("budget=0 should pass no reasoning option (resolved=0), got %d", fake.calls[0].reasoningBudget)
	}
}

func TestPostAsk_EmptyTagsSerializeAsEmptyArray(t *testing.T) {
	fake := &fakeLLM{replies: []string{
		`{"answer":"## Unknown\n\nI don't know.","tags":[]}`,
	}}
	h, _ := newTestHandlerWithLLM(t, fake)

	body, _ := json.Marshal(map[string]string{"question": "what is xyzzy?"})
	req := httptest.NewRequest(http.MethodPost, "/ask", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.PostAsk(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — body: %s", rec.Code, rec.Body.String())
	}
	// The raw body should contain "tags":[] not "tags":null — clients shouldn't
	// have to handle nil specially.
	if !strings.Contains(rec.Body.String(), `"tags":[]`) {
		t.Errorf("expected empty tags to serialize as [], body was: %s", rec.Body.String())
	}
}

func TestPostAsk_MalformedLLMReply(t *testing.T) {
	fake := &fakeLLM{replies: []string{`not json`}}
	h, _ := newTestHandlerWithLLM(t, fake)

	body, _ := json.Marshal(map[string]string{"question": "what?"})
	req := httptest.NewRequest(http.MethodPost, "/ask", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.PostAsk(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("expected 502 for malformed LLM JSON, got %d — body: %s", rec.Code, rec.Body.String())
	}
}

func TestPostAsk_EmptyQuestion(t *testing.T) {
	h, _ := newTestHandlerWithLLM(t, &fakeLLM{})

	body, _ := json.Marshal(map[string]string{"question": "   "})
	req := httptest.NewRequest(http.MethodPost, "/ask", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.PostAsk(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d — body: %s", rec.Code, rec.Body.String())
	}
}

func TestPostAsk_NoLLMConfigured(t *testing.T) {
	h, _ := newTestHandler(t) // nil LLM

	body, _ := json.Marshal(map[string]string{"question": "what is X?"})
	req := httptest.NewRequest(http.MethodPost, "/ask", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.PostAsk(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d — body: %s", rec.Code, rec.Body.String())
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
