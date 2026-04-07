package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/wiztools/knowledged/internal/queue"
	"github.com/wiztools/knowledged/internal/store"
)

func newTestHandler(t *testing.T) (*Handler, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	st, err := store.New(dir, logger)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	q, err := queue.New(st, nil, logger)
	if err != nil {
		t.Fatalf("queue.New: %v", err)
	}
	h := NewHandler(q, st, nil, logger)
	return h, st
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
