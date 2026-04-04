package organizer

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/wiztools/knowledged/internal/llm"
	"github.com/wiztools/knowledged/internal/store"
)

const systemPrompt = `You are a knowledge base organiser for a Git-backed Markdown store.
Your job is to decide where new content should be stored and whether any existing files
should be reorganised to keep the structure clean and coherent.

Rules:
- File paths must use kebab-case, end with .md, and be at most 3 levels deep.
- Suggest refactors only when they meaningfully improve organisation.
- Keep the updated INDEX.md clean: one entry per file, grouped by topic, each entry on its own line:
    - [Title](path/to/file.md) — one-line description
- Respond with ONLY valid JSON — no explanation, no markdown fences.`

const decidePromptTemplate = `Current INDEX.md:
---
%s
---

New content to store:
---
%s
---
%s
Decide where to store this content and return ONLY this JSON:
{
  "target_path": "category/subcategory/title.md",
  "title": "Document Title",
  "description": "One-line description",
  "refactors": [
    {"from": "old/path.md", "to": "new/path.md"}
  ],
  "updated_index": "<full updated INDEX.md content>"
}`

// Refactor describes a single file-move operation.
type Refactor struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Decision is the structured result of an LLM placement query.
type Decision struct {
	TargetPath   string     `json:"target_path"`
	Title        string     `json:"title"`
	Description  string     `json:"description"`
	Refactors    []Refactor `json:"refactors"`
	UpdatedIndex string     `json:"updated_index"`
}

// Organizer uses an LLM to decide where to place content and then applies
// that decision to the store.
type Organizer struct {
	store  *store.Store
	llm    llm.Provider
	logger *slog.Logger
}

// New creates an Organizer.
func New(st *store.Store, provider llm.Provider, logger *slog.Logger) *Organizer {
	return &Organizer{store: st, llm: provider, logger: logger}
}

// Decide asks the LLM where to store content and whether to refactor existing files.
func (o *Organizer) Decide(ctx context.Context, content, hint string, tags []string) (*Decision, error) {
	index, err := o.store.ReadIndex()
	if err != nil {
		return nil, fmt.Errorf("reading index: %w", err)
	}

	// Build optional metadata section.
	var meta strings.Builder
	if hint != "" {
		fmt.Fprintf(&meta, "\nHint from user: %s\n", hint)
	}
	if len(tags) > 0 {
		fmt.Fprintf(&meta, "Tags: %s\n", strings.Join(tags, ", "))
	}

	userPrompt := fmt.Sprintf(decidePromptTemplate, index, content, meta.String())

	o.logger.Debug("asking LLM for placement decision",
		"index_len", len(index),
		"content_len", len(content))

	raw, err := o.llm.Complete(ctx, systemPrompt, userPrompt)
	if err != nil {
		return nil, fmt.Errorf("LLM placement decision: %w", err)
	}

	o.logger.Debug("received placement decision from LLM", "response_len", len(raw))

	decision, err := parseDecision(raw)
	if err != nil {
		return nil, fmt.Errorf("parsing LLM decision: %w\nraw response:\n%s", err, raw)
	}

	o.logger.Info("LLM placement decision",
		"target_path", decision.TargetPath,
		"refactors", len(decision.Refactors))

	return decision, nil
}

// Execute applies a Decision to the store: moves files, writes the new document,
// updates INDEX.md, and commits everything in one atomic Git commit.
// The jobID is embedded in the commit message for crash-recovery purposes.
func (o *Organizer) Execute(ctx context.Context, jobID, content string, decision *Decision) error {
	// 1. Apply refactors first.
	for _, ref := range decision.Refactors {
		if !o.store.FileExists(ref.From) {
			o.logger.Warn("refactor source does not exist, skipping", "from", ref.From, "to", ref.To)
			continue
		}
		o.logger.Info("applying refactor", "from", ref.From, "to", ref.To)
		if err := o.store.MoveFile(ref.From, ref.To); err != nil {
			return fmt.Errorf("refactor %s → %s: %w", ref.From, ref.To, err)
		}
	}

	// 2. Write the new content file.
	o.logger.Info("writing content file", "path", decision.TargetPath)
	if err := o.store.WriteFile(decision.TargetPath, content); err != nil {
		return fmt.Errorf("writing content: %w", err)
	}

	// 3. Update INDEX.md.
	o.logger.Info("updating INDEX.md")
	if err := o.store.WriteIndex(decision.UpdatedIndex); err != nil {
		return fmt.Errorf("updating index: %w", err)
	}

	// 4. Single atomic commit — job ID embedded for crash recovery.
	msg := fmt.Sprintf("store(%s): %s", jobID, decision.TargetPath)
	if err := o.store.Commit(msg); err != nil {
		return fmt.Errorf("committing: %w", err)
	}

	return nil
}

// parseDecision extracts JSON from the LLM response, tolerating markdown fences.
func parseDecision(raw string) (*Decision, error) {
	raw = strings.TrimSpace(raw)

	// Strip ```json ... ``` or ``` ... ``` wrappers if present.
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

	var d Decision
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		return nil, fmt.Errorf("json unmarshal: %w", err)
	}
	if d.TargetPath == "" {
		return nil, fmt.Errorf("LLM returned empty target_path")
	}
	if d.UpdatedIndex == "" {
		return nil, fmt.Errorf("LLM returned empty updated_index")
	}
	return &d, nil
}
