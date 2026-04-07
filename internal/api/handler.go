package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/wiztools/knowledged/internal/llm"
	"github.com/wiztools/knowledged/internal/queue"
	"github.com/wiztools/knowledged/internal/store"
)

// Handler holds the dependencies shared across HTTP endpoints.
type Handler struct {
	queue  *queue.Queue
	store  *store.Store
	llm    llm.Provider
	logger *slog.Logger
}

// NewHandler creates a Handler.
func NewHandler(q *queue.Queue, st *store.Store, provider llm.Provider, logger *slog.Logger) *Handler {
	return &Handler{queue: q, store: st, llm: provider, logger: logger}
}

// ── POST /content ────────────────────────────────────────────────────────────

type postContentRequest struct {
	Content string   `json:"content"`
	Hint    string   `json:"hint,omitempty"`
	Tags    []string `json:"tags,omitempty"`
}

type postContentResponse struct {
	JobID  string `json:"job_id"`
	Status string `json:"status"`
}

// PostContent enqueues a store request and returns a job ID immediately.
func (h *Handler) PostContent(w http.ResponseWriter, r *http.Request) {
	var req postContentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Content) == "" {
		h.writeError(w, http.StatusBadRequest, "content must not be empty")
		return
	}

	h.logger.Info("received POST /content request",
		"hint", req.Hint,
		"tags", req.Tags,
		"content_len", len(req.Content))

	job, err := h.queue.Enqueue(req.Content, req.Hint, req.Tags)
	if err != nil {
		h.logger.Error("failed to enqueue job", "error", err)
		h.writeError(w, http.StatusInternalServerError, "failed to enqueue: "+err.Error())
		return
	}

	h.writeJSON(w, http.StatusAccepted, postContentResponse{
		JobID:  job.ID,
		Status: string(job.Status),
	})
}

// ── DELETE /content ──────────────────────────────────────────────────────────

type deleteContentRequest struct {
	Path string `json:"path"`
}

// DeleteContent enqueues a delete request and returns a job ID immediately.
func (h *Handler) DeleteContent(w http.ResponseWriter, r *http.Request) {
	var req deleteContentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Path) == "" {
		h.writeError(w, http.StatusBadRequest, "path must not be empty")
		return
	}

	h.logger.Info("received DELETE /content request", "path", req.Path)

	job, err := h.queue.EnqueueDelete(req.Path)
	if err != nil {
		h.logger.Error("failed to enqueue delete job", "error", err)
		h.writeError(w, http.StatusInternalServerError, "failed to enqueue: "+err.Error())
		return
	}

	h.logger.Info("delete job enqueued", "job_id", job.ID, "path", req.Path)

	h.writeJSON(w, http.StatusAccepted, postContentResponse{
		JobID:  job.ID,
		Status: string(job.Status),
	})
}

// ── GET /jobs/{id} ───────────────────────────────────────────────────────────

type jobStatusResponse struct {
	JobID  string `json:"job_id"`
	Status string `json:"status"`
	Path   string `json:"path,omitempty"`
	Error  string `json:"error,omitempty"`
}

// GetJob returns the current status of a job by ID.
func (h *Handler) GetJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		h.writeError(w, http.StatusBadRequest, "missing job id")
		return
	}

	job, ok := h.queue.GetJob(id)
	if !ok {
		h.writeError(w, http.StatusNotFound, "job not found: "+id)
		return
	}

	h.writeJSON(w, http.StatusOK, jobStatusResponse{
		JobID:  job.ID,
		Status: string(job.Status),
		Path:   job.Path,
		Error:  job.Error,
	})
}

// ── GET /content ─────────────────────────────────────────────────────────────
//
// Query parameters:
//   path=<repo-relative path>  → return raw file content
//   query=<text>               → synthesize (default) or raw document list
//   mode=raw|synthesize        → explicit mode override (only meaningful with query=)

type rawDocResponse struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type synthesisResponse struct {
	Query   string   `json:"query"`
	Sources []string `json:"sources"`
	Answer  string   `json:"answer"`
}

// GetContent serves content either as raw files or as an LLM-synthesized answer.
func (h *Handler) GetContent(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	query := r.URL.Query().Get("query")
	mode := strings.ToLower(r.URL.Query().Get("mode"))

	switch {
	case path != "":
		h.getRawFile(w, path)

	case query != "" && mode == "raw":
		h.getRawDocs(w, r.Context(), query)

	case query != "":
		h.getSynthesis(w, r.Context(), query)

	default:
		h.writeError(w, http.StatusBadRequest,
			"provide either path=<file> or query=<text> (optional mode=raw|synthesize)")
	}
}

// getRawFile returns the content of a single file.
func (h *Handler) getRawFile(w http.ResponseWriter, relPath string) {
	h.logger.Info("GET /content raw file", "path", relPath)

	content, err := h.store.ReadFile(relPath)
	if err != nil {
		h.logger.Warn("file not found", "path", relPath, "error", err)
		h.writeError(w, http.StatusNotFound, "file not found: "+relPath)
		return
	}

	h.writeJSON(w, http.StatusOK, rawDocResponse{Path: relPath, Content: content})
}

// getRawDocs uses the LLM to find relevant documents and returns them verbatim.
func (h *Handler) getRawDocs(w http.ResponseWriter, ctx context.Context, query string) {
	h.logger.Info("GET /content raw query", "query", query)

	paths, err := h.findRelevantPaths(ctx, query)
	if err != nil {
		h.logger.Error("failed to find relevant paths", "query", query, "error", err)
		h.writeError(w, http.StatusInternalServerError, "search failed: "+err.Error())
		return
	}

	var docs []rawDocResponse
	for _, p := range paths {
		content, err := h.store.ReadFile(p)
		if err != nil {
			h.logger.Warn("could not read relevant file", "path", p, "error", err)
			continue
		}
		docs = append(docs, rawDocResponse{Path: p, Content: content})
	}
	h.writeJSON(w, http.StatusOK, docs)
}

// getSynthesis uses the LLM to answer a query based on relevant documents.
func (h *Handler) getSynthesis(w http.ResponseWriter, ctx context.Context, query string) {
	h.logger.Info("GET /content synthesis", "query", query)

	paths, err := h.findRelevantPaths(ctx, query)
	if err != nil {
		h.logger.Error("failed to find relevant paths", "query", query, "error", err)
		h.writeError(w, http.StatusInternalServerError, "search failed: "+err.Error())
		return
	}

	if len(paths) == 0 {
		h.writeJSON(w, http.StatusOK, synthesisResponse{
			Query:   query,
			Sources: []string{},
			Answer:  "No relevant documents found in the knowledge base.",
		})
		return
	}

	// Build context from all relevant documents.
	var sb strings.Builder
	var readPaths []string
	for _, p := range paths {
		content, err := h.store.ReadFile(p)
		if err != nil {
			h.logger.Warn("could not read relevant file", "path", p, "error", err)
			continue
		}
		fmt.Fprintf(&sb, "=== %s ===\n%s\n\n", p, content)
		readPaths = append(readPaths, p)
	}

	system := `You are a knowledge base assistant. Answer the user's question based solely
on the documents provided. Be concise and accurate. If the documents do not
contain enough information, say so clearly.`

	userPrompt := fmt.Sprintf("Documents:\n\n%s\nQuestion: %s", sb.String(), query)

	h.logger.Debug("calling LLM for synthesis",
		"doc_count", len(readPaths),
		"context_len", sb.Len())

	answer, err := h.llm.Complete(ctx, system, userPrompt)
	if err != nil {
		h.logger.Error("LLM synthesis failed", "error", err)
		h.writeError(w, http.StatusInternalServerError, "synthesis failed: "+err.Error())
		return
	}

	h.writeJSON(w, http.StatusOK, synthesisResponse{
		Query:   query,
		Sources: readPaths,
		Answer:  answer,
	})
}

// ── helpers ──────────────────────────────────────────────────────────────────

const findRelevantSystem = `You are a knowledge base search assistant.
Given an index of documents and a query, identify the most relevant document paths.
Respond with ONLY valid JSON — no explanation, no markdown fences.`

type relevanceResponse struct {
	Paths       []string `json:"paths"`
	Explanation string   `json:"explanation"`
}

// findRelevantPaths asks the LLM which documents are relevant for a query.
func (h *Handler) findRelevantPaths(ctx context.Context, query string) ([]string, error) {
	index, err := h.store.ReadIndex()
	if err != nil {
		return nil, fmt.Errorf("reading index: %w", err)
	}

	userPrompt := fmt.Sprintf(`INDEX.md:
---
%s
---

Query: %s

Return ONLY this JSON (at most 5 paths, empty array if nothing is relevant):
{"paths": ["path/to/file.md"], "explanation": "why these match"}`, index, query)

	raw, err := h.llm.Complete(ctx, findRelevantSystem, userPrompt)
	if err != nil {
		return nil, fmt.Errorf("LLM relevance call: %w", err)
	}

	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "```") {
		lines := strings.SplitN(raw, "\n", 2)
		if len(lines) == 2 {
			raw = lines[1]
		}
		if idx := strings.LastIndex(raw, "```"); idx != -1 {
			raw = raw[:idx]
		}
		raw = strings.TrimSpace(raw)
	}

	var rel relevanceResponse
	if err := json.Unmarshal([]byte(raw), &rel); err != nil {
		return nil, fmt.Errorf("parsing relevance response: %w (raw: %s)", err, raw)
	}

	h.logger.Debug("relevance decision", "paths", rel.Paths, "explanation", rel.Explanation)

	// Filter out paths that do not actually exist.
	var valid []string
	for _, p := range rel.Paths {
		if h.store.FileExists(p) {
			valid = append(valid, p)
		} else {
			h.logger.Warn("LLM suggested non-existent path, ignoring", "path", p)
		}
	}
	return valid, nil
}

func (h *Handler) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		h.logger.Error("failed to encode JSON response", "error", err)
	}
}

func (h *Handler) writeError(w http.ResponseWriter, status int, msg string) {
	h.writeJSON(w, status, map[string]string{"error": msg})
}
