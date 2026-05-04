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
	"github.com/wiztools/knowledged/internal/recentlog"
	"github.com/wiztools/knowledged/internal/store"
)

// Handler holds the dependencies shared across HTTP endpoints.
type Handler struct {
	queue     *queue.Queue
	store     *store.Store
	llm       llm.Provider
	recentLog *recentlog.RecentLog
	logger    *slog.Logger
}

// NewHandler creates a Handler.
func NewHandler(q *queue.Queue, st *store.Store, provider llm.Provider, rl *recentlog.RecentLog, logger *slog.Logger) *Handler {
	return &Handler{queue: q, store: st, llm: provider, recentLog: rl, logger: logger}
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

// getSynthesis answers a query using the LLM. Snippet-first: the model sees
// truncated previews of each candidate doc, plus an escape hatch — it can
// either return its answer as prose, or return JSON `{"need_full": [...]}`
// to request the full body of specific docs and we recall it once with those.
// Cheap when snippets are enough; correct when they aren't.
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

	// Pass 1: build a snippet-only context. Include enough preview to answer
	// straightforward questions without the full body.
	var docs []synthesisDoc
	var readPaths []string
	for _, p := range paths {
		content, err := h.store.ReadFile(p)
		if err != nil {
			h.logger.Warn("could not read relevant file", "path", p, "error", err)
			continue
		}
		docs = append(docs, synthesisDoc{path: p, full: content, snippet: snippet(content, snippetCharBudget)})
		readPaths = append(readPaths, p)
	}

	answer, err := h.synthesizeWithSnippets(ctx, query, docs)
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

const (
	snippetCharBudget = 600 // per-doc preview budget for the snippets pass
)

// synthesisDoc is a candidate document staged for the snippet-first flow.
// path is the repo-relative file location, full is the entire body (read
// once and reused if the model asks for it), snippet is the truncated
// preview shown in the first call.
type synthesisDoc struct {
	path    string
	full    string
	snippet string
}

// synthesizeWithSnippets runs the snippet-first synthesis flow. May make a
// second call if the model requests full bodies for specific paths.
func (h *Handler) synthesizeWithSnippets(ctx context.Context, query string, docs []synthesisDoc) (string, error) {
	const system = `You are a knowledge base assistant. Answer the user's question
based solely on the documents provided. Be concise and accurate. If you can
answer from the snippets given, do so. If you genuinely need the full body of
specific documents to answer, ask for them via the escape hatch — don't guess
beyond what's in front of you.`

	var sb strings.Builder
	for _, d := range docs {
		fmt.Fprintf(&sb, "=== %s ===\n%s\n\n", d.path, d.snippet)
	}

	prompt := fmt.Sprintf(`Document snippets (truncated previews):

%s
Question: %s

You may respond in EITHER of two ways:

1. If the snippets are enough, return your prose answer directly. Plain text, no JSON, no markdown fences.

2. If you need the full body of one or more documents to answer correctly, return ONLY this JSON object — no prose, no markdown fences:
   {"need_full": ["path/a.md", "path/b.md"]}   // list ONLY the paths whose full body you actually need; max 3.

Default to option 1 unless the snippets clearly leave the question unanswerable.`, sb.String(), query)

	h.logger.Debug("LLM synthesis pass 1 (snippets)",
		"doc_count", len(docs),
		"context_len", sb.Len())

	resp, err := h.llm.Complete(ctx, system, prompt)
	if err != nil {
		return "", fmt.Errorf("snippets pass: %w", err)
	}

	needPaths, ok := parseNeedFull(resp)
	if !ok {
		return resp, nil
	}

	// Pass 2: recall with full bodies of just the requested docs.
	wanted := make(map[string]bool, len(needPaths))
	for _, p := range needPaths {
		wanted[p] = true
	}
	var fullSb strings.Builder
	for _, d := range docs {
		if !wanted[d.path] {
			continue
		}
		fmt.Fprintf(&fullSb, "=== %s ===\n%s\n\n", d.path, d.full)
	}

	if fullSb.Len() == 0 {
		// Model asked for paths that aren't in our candidate set — fall back
		// to giving it everything in full and a synthesis-only instruction.
		h.logger.Warn("model requested unknown paths, falling back to full bodies of all candidates",
			"requested", needPaths)
		for _, d := range docs {
			fmt.Fprintf(&fullSb, "=== %s ===\n%s\n\n", d.path, d.full)
		}
	}

	prompt2 := fmt.Sprintf(`Documents (full bodies):

%s
Question: %s

Answer the question directly. Plain text, no JSON, no fences.`, fullSb.String(), query)

	h.logger.Debug("LLM synthesis pass 2 (full bodies)",
		"requested_paths", needPaths,
		"context_len", fullSb.Len())

	answer, err := h.llm.Complete(ctx, system, prompt2)
	if err != nil {
		return "", fmt.Errorf("full-body pass: %w", err)
	}
	return answer, nil
}

// snippet returns a truncated preview of content. If content fits in budget,
// returns it unchanged. Otherwise returns the leading budget chars plus a
// truncation marker.
func snippet(content string, budget int) string {
	if len(content) <= budget {
		return content
	}
	return strings.TrimRight(content[:budget], " \t\n") + "\n…[truncated; full body available on request]"
}

// parseNeedFull tries to interpret resp as `{"need_full": [...]}`. Returns
// the paths and true when the response is exactly that JSON shape (after
// fence stripping). Returns false for any prose response, including JSON
// that doesn't have a need_full key.
func parseNeedFull(resp string) ([]string, bool) {
	cleaned := strings.TrimSpace(resp)
	if strings.HasPrefix(cleaned, "```") {
		if nl := strings.Index(cleaned, "\n"); nl >= 0 {
			cleaned = cleaned[nl+1:]
		}
		if idx := strings.LastIndex(cleaned, "```"); idx >= 0 {
			cleaned = cleaned[:idx]
		}
		cleaned = strings.TrimSpace(cleaned)
	}
	if !strings.HasPrefix(cleaned, "{") {
		return nil, false
	}
	var probe struct {
		NeedFull []string `json:"need_full"`
	}
	if err := json.Unmarshal([]byte(cleaned), &probe); err != nil {
		return nil, false
	}
	if len(probe.NeedFull) == 0 {
		return nil, false
	}
	return probe.NeedFull, true
}

// ── GET /posts/recents ─────────────────────────────────────────────────────────

type recentPostsResponse struct {
	Posts []recentlog.Entry `json:"posts"`
}

// GetRecentPosts returns the 20 most recently stored documents, newest first.
func (h *Handler) GetRecentPosts(w http.ResponseWriter, r *http.Request) {
	h.logger.Info("GET /posts/recents")

	entries, err := h.recentLog.Recent(20)
	if err != nil {
		h.logger.Error("recentlog read failed", "error", err)
		h.writeError(w, http.StatusInternalServerError, "failed to read recent posts: "+err.Error())
		return
	}
	if entries == nil {
		entries = []recentlog.Entry{}
	}
	h.writeJSON(w, http.StatusOK, recentPostsResponse{Posts: entries})
}

// ── helpers ──────────────────────────────────────────────────────────────────

// findRelevantPaths runs a two-pass drill-down to identify documents
// relevant to query without ever sending the full INDEX to the LLM.
// Both passes use structured output: the schemas constrain the reply shape
// and the per-field descriptions (in the schemas below) carry the rules.
//
// Pass 1 (route): the model sees only `## Heading (N entries)` lines and
// picks 1-3 sections likely to contain the answer.
// Pass 2 (paths): the model sees the bullets of just those sections and
// returns up to 5 specific file paths.
//
// Existence checks happen at the end — hallucinated paths are silently dropped.
func (h *Handler) findRelevantPaths(ctx context.Context, query string) ([]string, error) {
	indexRaw, err := h.store.ReadIndex()
	if err != nil {
		return nil, fmt.Errorf("reading index: %w", err)
	}
	parsed := store.ParseIndex(indexRaw)
	if len(parsed.Sections) == 0 {
		return nil, nil
	}

	// Pass 1: route to candidate sections.
	const routeSystem = `You are the routing step of a knowledge-base search.
Given a list of section headings, pick which subtrees most likely contain
the answer. You are NOT picking files — that happens in the next step.`

	headings := renderHeadingList(parsed)
	routePrompt := fmt.Sprintf(`Existing sections in INDEX.md (heading → bullet count):
%s
Query: %s`, headings, query)

	routeRaw, err := h.llm.CompleteStructured(ctx, routeSystem, routePrompt, searchRouteSchema())
	if err != nil {
		return nil, fmt.Errorf("LLM route call: %w", err)
	}
	var route struct {
		Sections []string `json:"sections"`
	}
	if err := json.Unmarshal([]byte(routeRaw), &route); err != nil {
		return nil, fmt.Errorf("parsing route response: %w (raw: %s)", err, routeRaw)
	}
	h.logger.Debug("relevance route decision", "sections", route.Sections)
	if len(route.Sections) == 0 {
		return nil, nil
	}

	// Pass 2: drill into the chosen subtrees and ask for specific paths.
	const pickSystem = `You are the path-picking step of a knowledge-base search.
You see only the sections that the routing step pre-selected. Return the
specific file paths most relevant to the query.`

	subtree := parsed.SubtreeFor(route.Sections)
	pickPrompt := fmt.Sprintf(`Relevant sections of INDEX.md:
---
%s
---

Query: %s`, subtree, query)

	pickRaw, err := h.llm.CompleteStructured(ctx, pickSystem, pickPrompt, searchPickSchema())
	if err != nil {
		return nil, fmt.Errorf("LLM pick call: %w", err)
	}
	var rel struct {
		Paths       []string `json:"paths"`
		Explanation string   `json:"explanation"`
	}
	if err := json.Unmarshal([]byte(pickRaw), &rel); err != nil {
		return nil, fmt.Errorf("parsing pick response: %w (raw: %s)", err, pickRaw)
	}
	h.logger.Debug("relevance pick decision", "paths", rel.Paths, "explanation", rel.Explanation)

	if len(rel.Paths) > 5 {
		rel.Paths = rel.Paths[:5]
	}
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

// searchRouteSchema constrains pass 1 of findRelevantPaths.
func searchRouteSchema() llm.Schema {
	return llm.Schema{
		Name:        "search_route",
		Description: "Pick which sections to search.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"sections": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"maxItems":    3,
					"description": "1-3 section names from the list above whose subtree to search. Empty list ONLY if no section is plausibly relevant.",
				},
			},
			"required":             []string{"sections"},
			"additionalProperties": false,
		},
	}
}

// searchPickSchema constrains pass 2 of findRelevantPaths.
func searchPickSchema() llm.Schema {
	return llm.Schema{
		Name:        "search_pick",
		Description: "Pick relevant file paths.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"paths": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"maxItems":    5,
					"description": "Up to 5 specific file paths drawn from the bullets above. Empty list if nothing in these sections actually matches.",
				},
				"explanation": map[string]any{
					"type":        "string",
					"description": "One short sentence on why; for logs only.",
				},
			},
			"required":             []string{"paths", "explanation"},
			"additionalProperties": false,
		},
	}
}

// renderHeadingList produces "- <heading> (<n> entries)" lines for the
// routing prompts. Mirrors organizer.renderHeadingList; kept local to avoid
// an import cycle with the organizer package.
func renderHeadingList(p store.ParsedIndex) string {
	if len(p.Sections) == 0 {
		return "(none — INDEX.md has no sections yet)"
	}
	var sb strings.Builder
	for _, s := range p.Sections {
		count := 0
		for _, line := range strings.Split(s.Body, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "- ") {
				count++
			}
		}
		fmt.Fprintf(&sb, "- %s (%d entries)\n", strings.TrimSpace(s.Name), count)
	}
	return sb.String()
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
