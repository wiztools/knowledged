package organizer

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/wiztools/knowledged/internal/llm"
	"github.com/wiztools/knowledged/internal/store"
)

// Two-pass organizer with structured output.
//
// Pass 1 (route): the model sees only the section headings — not the bullets —
//   plus the new content. It picks 1-3 existing sections to consider for
//   placement and may propose a new section.
// Pass 2 (decide): the model sees just those sections' bullets plus the new
//   content. It returns the target path, optional refactors limited to those
//   sections, and updated bodies for the touched sections (which get spliced
//   back into the full INDEX). The model never has to echo a 5KB index it
//   didn't change.
//
// Both passes use llm.CompleteStructured — the schemas enforce reply shape
// and the per-field descriptions live next to each field name (the strongest
// form of "rules at the decision boundary"). System prompts carry role only.

const routeSystemPrompt = `You are the routing step of a knowledge-base organizer.
Given a list of section headings, pick which ones the new content most likely
belongs to or near. You are NOT placing the file — a second step does that
with the full bullets of whatever you select.`

const routePromptTemplate = `Existing sections in INDEX.md (heading → bullet count):
%s

New content to store:
---
%s
---
%s`

const decideSystemPrompt = `You are the placement step of a knowledge-base organizer.
You see ONLY the sections of INDEX.md that the routing step pre-selected, plus
the new content. Decide where the file goes, propose any narrowly-scoped
refactors, and return updated bodies for ONLY the sections you actually
changed.`

const decidePromptTemplate = `Relevant sections of INDEX.md:
---
%s
---
%s
New content to store:
---
%s
---
%s`

// routeSchema constrains pass 1 to the {candidate_sections, proposed_new_section} shape.
func routeSchema() llm.Schema {
	return llm.Schema{
		Name:        "route_decision",
		Description: "Pick which existing sections the new content belongs in or near; optionally propose a new section.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"candidate_sections": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"maxItems":    3,
					"description": "1-3 existing section names from the list above; choose those whose subtree is most relevant for placing OR refactoring near the new content. Empty list ONLY if the index has no sections at all.",
				},
				"proposed_new_section": map[string]any{
					"type":        "string",
					"description": "Optional; non-empty if no existing section fits well — propose a short Title-Case heading for a brand-new section. Empty string otherwise.",
				},
			},
			"required":             []string{"candidate_sections", "proposed_new_section"},
			"additionalProperties": false,
		},
	}
}

// placementSchema constrains pass 2 to the full placement decision shape.
func placementSchema() llm.Schema {
	return llm.Schema{
		Name:        "placement_decision",
		Description: "Final placement: target path, refactors, and updated section bodies.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"target_path": map[string]any{
					"type":        "string",
					"description": "Required. Lowercase kebab-case. Ends with .md. 2-3 path segments only (no deeper nesting). Slashes only.",
				},
				"title": map[string]any{
					"type":        "string",
					"description": "Required. Concise human title (under 80 chars).",
				},
				"description": map[string]any{
					"type":        "string",
					"description": "Required. One sentence, under 120 chars, summarizing the NOTE'S SUBJECT MATTER — what the note is about, readable standalone. Appears in the INDEX entry. Do NOT narrate your placement or refactor actions here (creating a section, de-duplicating, moving files); describe the content only.",
				},
				"tags": map[string]any{
					"type":        "array",
					"description": "Required. If tags were provided by the user, return exactly those tags. Otherwise generate 1-5 short lowercase tags suitable for cataloging this note.",
					"items":       map[string]any{"type": "string"},
				},
				"refactors": map[string]any{
					"type":        "array",
					"description": "Only file moves WITHIN the sections shown above. Always include this key; emit an empty array unless a move meaningfully improves placement.",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"from": map[string]any{"type": "string"},
							"to":   map[string]any{"type": "string"},
						},
						"required":             []string{"from", "to"},
						"additionalProperties": false,
					},
				},
				"updated_sections": map[string]any{
					"type":        "array",
					"minItems":    1,
					"description": "Required. One entry per section you touched (including any new section you create).",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"name": map[string]any{
								"type":        "string",
								"description": "Section heading text (without the `## ` prefix).",
							},
							"body": map[string]any{
								"type":        "string",
								"description": "ONE bullet line per file in the form: - [Title](path.md) — description\\n",
							},
						},
						"required":             []string{"name", "body"},
						"additionalProperties": false,
					},
				},
			},
			"required":             []string{"target_path", "title", "description", "tags", "refactors", "updated_sections"},
			"additionalProperties": false,
		},
	}
}

// Refactor describes a single file-move operation.
type Refactor struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// updatedSection mirrors store.IndexSection but carries JSON tags.
type updatedSection struct {
	Name string `json:"name"`
	Body string `json:"body"`
}

// routeDecision is the pass-1 response.
type routeDecision struct {
	CandidateSections  []string `json:"candidate_sections"`
	ProposedNewSection string   `json:"proposed_new_section"`
}

// placementDecision is the pass-2 response.
type placementDecision struct {
	TargetPath      string           `json:"target_path"`
	Title           string           `json:"title"`
	Description     string           `json:"description"`
	Tags            []string         `json:"tags"`
	Refactors       []Refactor       `json:"refactors"`
	UpdatedSections []updatedSection `json:"updated_sections"`
}

// Decision is what the organizer hands back to the worker after both passes.
// UpdatedIndex is the full INDEX.md rebuilt by splicing the model's
// updated_sections into the existing index.
type Decision struct {
	TargetPath   string
	Title        string
	Description  string
	Tags         []string
	Created      time.Time
	Modified     time.Time
	Refactors    []Refactor
	UpdatedIndex string
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

// Decide runs the two-pass placement flow. The LLM never sees the full INDEX
// at once — pass 1 sees only headings, pass 2 sees only the selected sections.
func (o *Organizer) Decide(ctx context.Context, content, hint, title string, tags []string) (*Decision, error) {
	return o.DecideAvoiding(ctx, content, hint, title, tags, nil)
}

// DecideAvoiding runs the placement flow while telling the LLM to avoid target
// paths that already conflicted during this post.
func (o *Organizer) DecideAvoiding(ctx context.Context, content, hint, title string, tags []string, conflictingPaths []string) (*Decision, error) {
	rawIndex, err := o.store.ReadIndex()
	if err != nil {
		return nil, fmt.Errorf("reading index: %w", err)
	}
	parsed := store.ParseIndex(rawIndex)

	meta := buildMetaBlock(hint, title, tags)
	meta += buildConflictBlock(conflictingPaths)

	// ── Pass 1: routing ──────────────────────────────────────────────────
	headingList := renderHeadingList(parsed)
	routePrompt := fmt.Sprintf(routePromptTemplate, headingList, content, meta)

	o.logger.Debug("organizer pass 1: route",
		"section_count", len(parsed.Sections),
		"content_len", len(content))

	routeRaw, err := o.llm.CompleteStructured(ctx, routeSystemPrompt, routePrompt, routeSchema())
	if err != nil {
		return nil, fmt.Errorf("LLM route call: %w", err)
	}
	route, err := parseRoute(routeRaw)
	if err != nil {
		return nil, fmt.Errorf("parsing route response: %w\nraw:\n%s", err, routeRaw)
	}
	o.logger.Info("organizer route decision",
		"candidates", route.CandidateSections,
		"proposed_new", route.ProposedNewSection)

	// ── Pass 2: placement ────────────────────────────────────────────────
	subtree := parsed.SubtreeFor(route.CandidateSections)
	if strings.TrimSpace(subtree) == strings.TrimSpace(parsed.Header) {
		// No matching sections — either the index is empty or the model
		// hallucinated names. Send a placeholder note to keep the prompt sensible.
		subtree = "(no existing sections selected — the new file likely starts a fresh section)\n"
	}

	newSectionHint := ""
	if route.ProposedNewSection != "" {
		newSectionHint = fmt.Sprintf(
			"Routing step proposed creating a NEW section named %q if no existing section fits.\n",
			route.ProposedNewSection,
		)
	}

	decidePrompt := fmt.Sprintf(decidePromptTemplate, subtree, newSectionHint, content, meta)

	o.logger.Debug("organizer pass 2: decide",
		"subtree_len", len(subtree),
		"selected_sections", len(route.CandidateSections))

	decideRaw, err := o.llm.CompleteStructured(ctx, decideSystemPrompt, decidePrompt, placementSchema())
	if err != nil {
		return nil, fmt.Errorf("LLM decide call: %w", err)
	}
	placement, err := parsePlacement(decideRaw)
	if err != nil {
		return nil, fmt.Errorf("parsing decide response: %w\nraw:\n%s", err, decideRaw)
	}

	resolvedTitle := strings.TrimSpace(title)
	if resolvedTitle == "" {
		resolvedTitle = strings.TrimSpace(placement.Title)
	}
	if resolvedTitle == "" {
		return nil, fmt.Errorf("LLM returned empty title")
	}

	resolvedTags := cleanTags(tags)
	if len(resolvedTags) == 0 {
		resolvedTags = cleanTags(placement.Tags)
	}

	// Splice the updated sections back into the full index.
	updates := make([]store.IndexSection, 0, len(placement.UpdatedSections))
	for _, u := range placement.UpdatedSections {
		u.Body = rewriteIndexEntryMetadata(u.Body, placement.TargetPath, resolvedTitle, placement.Description)
		updates = append(updates, store.IndexSection{Name: u.Name, Body: u.Body})
	}
	mergedIndex := parsed.ReplaceSections(updates).Render()

	o.logger.Info("organizer placement decision",
		"target_path", placement.TargetPath,
		"refactors", len(placement.Refactors),
		"sections_updated", len(updates))

	return &Decision{
		TargetPath:   placement.TargetPath,
		Title:        resolvedTitle,
		Description:  placement.Description,
		Tags:         resolvedTags,
		Refactors:    placement.Refactors,
		UpdatedIndex: mergedIndex,
	}, nil
}

// Execute applies a Decision to the store: moves files, writes the new document,
// updates INDEX.md, and commits everything in one atomic Git commit.
// The jobID is embedded in the commit message for crash-recovery purposes.
func (o *Organizer) Execute(ctx context.Context, jobID, content string, decision *Decision) error {
	if o.store.FileExists(decision.TargetPath) {
		return fmt.Errorf("writing content: %w: %s", store.ErrFileExists, decision.TargetPath)
	}

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

	o.logger.Info("writing content file", "path", decision.TargetPath)
	body, err := store.StripFrontmatter(content)
	if err != nil {
		return fmt.Errorf("parsing inbound content frontmatter: %w", err)
	}
	created := decision.Created
	if created.IsZero() {
		created = time.Now().UTC()
	}
	modified := decision.Modified
	if modified.IsZero() {
		modified = created
	}
	rendered := store.RenderFrontmatter(store.Frontmatter{
		Title:       decision.Title,
		Description: decision.Description,
		Tags:        decision.Tags,
		Created:     created,
		Modified:    modified,
	}, body)
	if err := o.store.WriteNewFile(decision.TargetPath, rendered); err != nil {
		return fmt.Errorf("writing content: %w", err)
	}

	o.logger.Info("updating INDEX.md")
	if err := o.store.WriteIndex(decision.UpdatedIndex); err != nil {
		return fmt.Errorf("updating index: %w", err)
	}

	msg := fmt.Sprintf("store(%s): %s", jobID, decision.TargetPath)
	if err := o.store.Commit(msg); err != nil {
		return fmt.Errorf("committing: %w", err)
	}

	return nil
}

// renderHeadingList produces a compact "## Heading — N entries" listing for
// the routing prompt. Empty index → a placeholder line so the prompt makes sense.
func renderHeadingList(p store.ParsedIndex) string {
	if len(p.Sections) == 0 {
		return "(none — INDEX.md has no sections yet)"
	}
	var sb strings.Builder
	for _, s := range p.Sections {
		count := countBulletLines(s.Body)
		fmt.Fprintf(&sb, "- %s (%d entries)\n", strings.TrimSpace(s.Name), count)
	}
	return sb.String()
}

func countBulletLines(body string) int {
	n := 0
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "- ") {
			n++
		}
	}
	return n
}

func buildMetaBlock(hint, title string, tags []string) string {
	var sb strings.Builder
	if hint != "" {
		fmt.Fprintf(&sb, "\nHint from user: %s\n", hint)
	}
	if strings.TrimSpace(title) != "" {
		fmt.Fprintf(&sb, "Title from user: %s\nUse this exact title for the new document.\n", strings.TrimSpace(title))
	}
	cleanedTags := cleanTags(tags)
	if len(cleanedTags) > 0 {
		fmt.Fprintf(&sb, "Tags from user: %s\nUse these exact tags for the new document.\n", strings.Join(cleanedTags, ", "))
	} else {
		sb.WriteString("Tags from user: (none provided)\n")
	}
	return sb.String()
}

func cleanTags(in []string) []string {
	out := make([]string, 0, len(in))
	for _, tag := range in {
		if tag = strings.TrimSpace(tag); tag != "" {
			out = append(out, tag)
		}
	}
	return out
}

func rewriteIndexEntryMetadata(body, path, title, description string) string {
	lines := strings.Split(body, "\n")
	link := "](" + path + ")"
	for i, line := range lines {
		if !strings.Contains(line, link) {
			continue
		}
		next := "- [" + strings.TrimSpace(title) + "](" + path + ")"
		if strings.TrimSpace(description) != "" {
			next += " — " + strings.TrimSpace(description)
		}
		lines[i] = next
		break
	}
	return strings.Join(lines, "\n")
}

func buildConflictBlock(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\nPlacement conflict from previous attempt:\n")
	sb.WriteString("The following target path already exists and MUST NOT be reused or overwritten:\n")
	for _, path := range paths {
		fmt.Fprintf(&sb, "- %s\n", path)
	}
	sb.WriteString("Choose a different new target_path and update INDEX.md for that new path.\n")
	return sb.String()
}

// parseRoute / parsePlacement deserialise the structured-output JSON. The
// schema guarantees shape; we only re-validate values that the schema can't
// fully constrain (e.g. non-empty target_path).
func parseRoute(raw string) (*routeDecision, error) {
	var r routeDecision
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return nil, fmt.Errorf("json unmarshal: %w", err)
	}
	return &r, nil
}

func parsePlacement(raw string) (*placementDecision, error) {
	var p placementDecision
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return nil, fmt.Errorf("json unmarshal: %w", err)
	}
	if p.TargetPath == "" {
		return nil, fmt.Errorf("LLM returned empty target_path")
	}
	if len(p.UpdatedSections) == 0 {
		return nil, fmt.Errorf("LLM returned no updated_sections")
	}
	return &p, nil
}
