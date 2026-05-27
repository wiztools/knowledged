# YAML Frontmatter for Knowledged Notes

**Date:** 2026-05-27
**Status:** Draft

## Overview

Move per-note metadata (title, description, tags, timestamps) from sidecars and `INDEX.md` into YAML frontmatter inside each `.md` file. Make each note self-contained. Treat `INDEX.md` and `.knowledged/recent-posts.jsonl` as **derived** views that can be rebuilt from the files at any time.

## Motivation

Knowledged today scatters per-note metadata across three places:

| Source | Holds | Durability |
|---|---|---|
| `<note>.md` | Content only (no metadata) | Permanent, committed |
| `INDEX.md` | Title + one-line description | Permanent, committed; hand-managed by the organizer |
| `.knowledged/recent-posts.jsonl` | Tags + `created_at` + `job_id` | **Local-only** (`.knowledged/` is gitignored); **auto-compacted to the last 20 entries** |
| Server index | Tags + embeddings + content | Remote; depends on server state |

Three concrete problems follow:

1. **Tags are not durable in the repo.** `recentlog.go` compacts the JSONL to 20 entries (`keepCount = 20`). On a repo with 74 notes, tags for ~54 of them survive only on the server. Lose the server, lose the tags.
2. **Notes are not self-describing.** Cloning the repo to another machine without `.knowledged/`, or sharing a single `.md` file, drops all tags and timestamps. There is no path from `<note>.md` alone to its metadata.
3. **INDEX.md is hand-maintained source.** The organizer constructs an updated INDEX in the same LLM pass that picks the file's location, and writes it directly. Title and description live nowhere else. Drift between INDEX.md and per-file content is possible and silent.

Frontmatter solves all three: metadata lives inside the file, travels with it, and INDEX.md becomes a projection that can be rebuilt deterministically from files on disk.

## Schema

```yaml
---
title: GGUF Models
description: Overview of the GGUF binary format for storing and distributing LLMs locally.
tags: [ml, quantization, llama-cpp, local-inference]
created: 2026-04-15T10:22:31Z
modified: 2026-05-25T23:31:02Z
---

# GGUF Models

GGUF (GPT-Generated Unified Format) is a binary file format ...
```

| Key | Type | Required | Source today | Source after |
|---|---|---|---|---|
| `title` | string | yes | INDEX.md entry | Frontmatter (canonical) |
| `description` | string | yes | INDEX.md entry | Frontmatter (canonical) |
| `tags` | list of strings | optional | recent-posts.jsonl + server | Frontmatter (canonical) |
| `created` | RFC 3339 | yes | recent-posts.jsonl, git log | Frontmatter (canonical), backed by git on first write |
| `modified` | RFC 3339 | yes | git log | Frontmatter (canonical), updated on every edit |

**Body** retains the existing convention: starts with `# H1` matching `title`. Frontmatter is delimited by `---` lines per the YAML standard; parsers must skip it when computing content length or embedding chunks.

## Behavior changes by package

### `internal/store`

- New `internal/store/frontmatter.go`:
  - `Parse(content string) (Frontmatter, body string, err error)`
  - `Render(fm Frontmatter, body string) string`
  - Requires stored notes to have frontmatter after the one-time migration; missing frontmatter is an error.
- `WriteFile` paths that originate from `Organizer` and `kc edit` go through `Render` so every committed file has frontmatter.
- `internal/store/index.go` becomes thinner — `UpdateIndexEntry` and `parseIndexEntryLine` can be deleted once INDEX.md is rebuilt as a projection (see "INDEX.md becomes derived" below). Keep `RemoveIndexEntry` for delete flows or replace with rebuild.

### `internal/organizer`

- `placementSchema` already returns `title` and `description` — keep them.
- `Decision` carries `Title`, `Description`, `Tags`, `Created`, `Modified`. Tags come from the `kc post --tags` input (already passed through `buildMetaBlock`); on edit, tags are preserved from the existing file's frontmatter unless the caller overrides.
- `Execute` writes the file with frontmatter via `store.Render`, then regenerates INDEX.md from disk (see below), then commits.
- The LLM no longer authors INDEX.md updates. The `updated_sections` field can be removed from `placementSchema` — INDEX.md is now derived. **However**, the LLM still needs to see the existing INDEX for routing/placement context; we keep `routeSchema` and the headings-only routing prompt unchanged. The decide pass returns only `target_path`, `title`, `description`, `refactors`.

### `internal/recentlog`

- Becomes a thin "recent activity" log: `job_id`, `path`, `created_at`. Tags drop out (they're in the file now).
- Compaction policy (20-entry cap) is fine for activity tail purposes. No durability concerns once tags move to frontmatter.
- Alternative considered: delete the package entirely and reconstruct recent activity from `git log`. Rejected for v1 — git log scans are slower than reading a 20-line JSONL, and `kc recent` is a hot path. Keep the file, shrink its schema.

### `cmd/kc`

- `kc post --tags ml,transformer` → tags flow into frontmatter via Organizer.
- `kc edit --title ... --description ...` updates frontmatter on the target file (instead of editing INDEX.md as a separate write).
- New flag `kc edit --tags ml,transformer` to retag without touching content.
- `kc recent` output gains `tags` field by reading the file's frontmatter (one extra parse per row, ≤ 20 rows).

### `cmd/knowledged` (server)

- On startup, optionally run a migration check: if any committed `.md` lacks frontmatter, log a warning. Migration itself is a separate one-shot job (below) — not run automatically on every start.

## INDEX.md becomes derived

INDEX.md today is hand-rolled inside the organizer. After this change:

- Treat `INDEX.md` as the output of a pure function `RebuildIndex(files []NoteWithFrontmatter) string`.
- The organizer calls `RebuildIndex` after writing a file and commits both the file and the regenerated INDEX in one commit.
- Section structure on INDEX.md is preserved (top-level folder → section heading, e.g. `## Notes`, `## AI`). The LLM still picks the *target path* (which determines section); the *bullet text* is mechanically generated from `title` + `description`.
- A new `kc reindex` subcommand rebuilds INDEX.md from disk for repair scenarios (after manual `.md` edits, after the migration, etc.).

This eliminates the entire `index.go` mutation API (`UpdateIndexEntry`, `parseIndexEntryLine`) in favor of a single rebuild step that's easier to reason about and impossible to drift.

## Migration

One-shot migration packaged as `kc migrate-frontmatter` (or a `cmd/knowledged-migrate` binary, depending on preference):

1. Read every `.md` under the repo root (excluding `INDEX.md` and `.knowledged/`).
2. For each file:
   - Skip if it already has frontmatter (idempotent).
   - Parse first `# H1` → `title`.
   - Look up the file's path in `INDEX.md` → `description`.
   - Look up the file in `recent-posts.jsonl` if present → `tags`, `created`. Fall back to:
     - `git log --diff-filter=A --format=%aI -- <file>` → `created`
     - `git log -1 --format=%aI -- <file>` → `modified`
     - Empty `tags: []` if no record exists.
   - Render with `store.Render` and write back.
3. Regenerate `INDEX.md` via `RebuildIndex` (cosmetic; should produce the same content if migration is correct).
4. Commit as `migrate: add frontmatter to all notes` in a single commit. No job UUID — this is operator-driven.

The migration is **idempotent and offline** — it does not need the knowledged server running. Tags that aren't in the local JSONL (because of the 20-entry cap) can optionally be backfilled from the server via `kc migrate-frontmatter --pull-tags-from-server`.

## Non-goals

- **No new metadata fields beyond the five above.** `draft`, `related`, `source_url`, `license` are tempting but out of scope. Add later as separate one-line spec amendments.
- **No support for custom Front-matter formats** (TOML, JSON). YAML only.
- **No schema versioning** in v1. If we change the schema later, add a `schema_version` key then.
- **No automatic re-tagging by the LLM on edits.** Tags persist across edits unless the user explicitly passes `--tags`.

## Risks and mitigations

| Risk | Mitigation |
|---|---|
| Existing consumers (kc CLI, MCP server, BHQ pipelines) read INDEX.md to discover notes | INDEX.md stays committed and well-formed; only its *authoring path* changes. Consumers don't notice. |
| Embedded LLM content quality changes (frontmatter is now in the file body the embedder sees) | Embedder splits at the second `---` and embeds only the body. Frontmatter is reflected separately as metadata fields on the indexed record. |
| Migration writes 74 files in one commit, polluting `git log` for that day | Acceptable — it's a one-time event with a clearly-named commit. Skip if the user prefers a per-file commit pass (slower but cleaner blame). |
| Hand-edited frontmatter goes out of sync with INDEX.md until `kc reindex` runs | Knowledged-driven writes always regenerate INDEX.md. Hand edits are the user's responsibility; a `kc reindex --check` flag can fail CI if drift is detected. |
| `title` in frontmatter and `# H1` in body disagree | Define `title` as the source of truth; `kc edit --title` updates frontmatter only and never rewrites the body. Drift is the user's choice. |

## Implementation order

1. `internal/store/frontmatter.go` — parser/renderer + tests (no behavior change yet).
2. `internal/store` — `RebuildIndex` function + tests.
3. `internal/organizer` — `Decide` and `Execute` route through frontmatter and `RebuildIndex`. `placementSchema` drops `updated_sections`. Update organizer tests.
4. `internal/recentlog` — drop `Tags` from `Entry`. Update callers and tests.
5. `cmd/kc` — `edit --tags`, `recent` output includes tags from frontmatter, new `reindex` subcommand.
6. Migration binary or subcommand.
7. Run migration against `subwiz/my-knowledged` as the canary.
8. Documentation: update `README.md` and `ARCHITECTURE.md`.

Each step is a separate PR. Steps 1–2 are safe to land independently; step 3 is the behavior switch.

## Out-of-scope downstream benefit

Once frontmatter ships, the planned `knowledged.to` publishing pipeline simplifies from ~200 lines of metadata synthesis to ~30 lines of "copy files, key-rename, push". Future downstream tools (Obsidian, Quartz, MkDocs, search exporters) can read knowledged-managed repos without translation. These are not justifications for the change — they're free side effects of making notes self-describing.
