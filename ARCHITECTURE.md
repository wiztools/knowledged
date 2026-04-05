# Architecture

## System overview

```
┌─────────────────────────────────────────────────────────┐
│                      HTTP Server                         │
│   POST /content   GET /content   GET /jobs/{id}          │
└────────┬──────────────┬──────────────────────────────────┘
         │              │
    write to         read-only
    queue.json       (concurrent ok)
         │              │
         ▼              ▼
┌────────────────┐  ┌──────────────────────────────────────┐
│  Queue Worker  │  │           Query Engine                │
│  (1 goroutine) │  │  - LLM: which docs are relevant?     │
│  serializes    │  │  - read matching files               │
│  all writes    │  │  - LLM: synthesise answer            │
└───────┬────────┘  └──────────────────────────────────────┘
        │
        ▼
┌───────────────────────────────────────────────────────┐
│                    Organizer                           │
│  1. read INDEX.md                                     │
│  2. LLM: target path + refactors + updated index      │
│  3. apply file moves (if any)                         │
│  4. write content file                                │
│  5. write INDEX.md                                    │
└───────────────────┬───────────────────────────────────┘
                    │
                    ▼
┌───────────────────────────────────────────────────────┐
│                  Store (go-git)                        │
│  WriteFile → worktree.Add                             │
│  MoveFile  → write dest + worktree.Remove src         │
│  Commit    → single atomic git commit per job         │
└───────────────────────────────────────────────────────┘
```

## Package responsibilities

| Package | Responsibility |
|---|---|
| `cmd/knowledged` | CLI flags, wires all components, starts HTTP server and queue worker |
| `cmd/kc` | CLI client — `post`, `get`, `job` subcommands; handles async polling |
| `internal/api` | HTTP handlers; routes GET/POST to queue or query engine |
| `internal/queue` | Durable job queue backed by `queue.json`; single worker goroutine |
| `internal/organizer` | Constructs LLM prompts, parses decisions, drives store operations |
| `internal/store` | go-git wrapper; file I/O, staging, committing, git log scan |
| `internal/llm` | `Provider` interface + Ollama implementation |

---

## Write path — `POST /content`

```
HTTP handler
  │
  ├─ validate request body (content must be non-empty)
  ├─ append job (status=queued) to queue.json   ← mutex protected
  ├─ send non-blocking signal on worker channel
  └─ return HTTP 202 { job_id, status: "queued" }

Worker goroutine (wakes on signal)
  │
  ├─ nextQueued(): read queue.json, find oldest queued job,
  │                mark it status=processing, rewrite file  ← mutex
  │
  ├─ Organizer.Decide()
  │    ├─ read INDEX.md
  │    ├─ build prompt (index + content + hint + tags)
  │    ├─ LLM.Complete()  →  raw JSON string
  │    └─ parse Decision { target_path, refactors, updated_index }
  │
  ├─ Organizer.Execute()
  │    ├─ for each refactor: Store.MoveFile(from, to)
  │    ├─ Store.WriteFile(target_path, content)
  │    ├─ Store.WriteIndex(updated_index)
  │    └─ Store.Commit("store(<jobID>): <target_path>")
  │                         ▲
  │                 job ID embedded — used for crash recovery
  │
  └─ finalize(): mark job done/failed in queue.json, cache in results map
```

---

## Read path — `GET /content`

Three modes, selected by query parameters:

```
?path=<rel-path>
  └─ Store.ReadFile(path) → { path, content }

?query=<text>&mode=raw
  ├─ LLM: given INDEX.md + query, which paths are relevant?  (≤5)
  ├─ filter out paths that don't exist on disk
  └─ read each file → [{ path, content }, ...]

?query=<text>  (default mode: synthesize)
  ├─ LLM: which paths are relevant?
  ├─ read each file, concatenate as context
  └─ LLM: answer query using those documents → { query, sources, answer }
```

Read operations do not acquire any lock — they access the filesystem directly via `os.ReadFile`. A GET may occasionally observe a partially-updated state (new file written, index not yet updated) during an active write. This is acceptable for v1.

---

## Queue durability and crash recovery

### Durability contract

- Every job is written to `queue.json` before `POST /content` returns — no job is silently lost on crash.
- `queue.json` is rewritten atomically: write to `queue.json.tmp`, then `os.Rename`. On POSIX this is a single syscall; the file is never in a partially-written state.
- `queue.json` is excluded from Git via `.gitignore` — it is operational state, not knowledge content.

### Job status transitions

```
queued ──► processing ──► done
                     └──► failed
```

The transition `queued → processing` is written to disk before any work begins. This makes the state machine recoverable after a crash.

### Startup reconciliation (`queue.reconcile`)

On every server start, the queue scans `queue.json` and resolves any non-terminal states:

```
done / failed   → load into in-memory results map, leave in file
queued          → re-signal worker channel
processing      → check git log for a commit containing the job ID
                    found  → mark done  (work completed before crash)
                    absent → reset to queued, retry
```

The git commit message `store(<jobID>): <path>` is the ground truth. It is written atomically by go-git — either the commit exists or it does not. This makes crash recovery exact: a job is never executed twice.

---

## Store initialisation

`store.New(repoPath)` handles four cases on startup:

| Directory state | Action |
|---|---|
| Does not exist | `os.MkdirAll` + `git init` + bootstrap |
| Exists, empty | `git init` + bootstrap |
| Exists, is a Git repo | Open; run `ensureBootstrapped` if index/gitignore missing |
| Exists, non-empty, not a Git repo | Return error — refuse to touch it |

**Bootstrap** creates two files and makes the initial commit:
- `.gitignore` — contains `/kc` and `/knowledged` (root binaries) plus `queue.json`
- `INDEX.md` — empty scaffold with auto-management comment

**`ensureBootstrapped`** (for pre-existing repos) checks each file individually and only commits if something was missing, leaving existing content untouched.

---

## LLM integration

### Interface

```go
type Provider interface {
    Complete(ctx context.Context, system, user string) (string, error)
}
```

The organizer and query engine depend only on this interface. Adding a new backend (Anthropic, OpenAI, etc.) requires implementing one method.

### Ollama transport

- Endpoint: `POST <ollama-url>/api/chat`
- Payload: `{ model, messages: [{role, content}, ...], stream: false }`
- HTTP timeout: 120 s (LLM inference can be slow for large contexts)
- Response: `message.content` string extracted from the JSON body

### Organizer prompt contract

The organizer sends one LLM call per job:

**System prompt** — sets role and rules (kebab-case paths, max 3 levels, minimal refactors, JSON-only output).

**User prompt** — contains:
1. Current `INDEX.md` content
2. The content to store
3. Optional hint and tags

**Expected response** — strict JSON, no markdown fences:

```json
{
  "target_path":   "category/subcategory/title.md",
  "title":         "Document Title",
  "description":   "One-line description",
  "refactors":     [{ "from": "old/path.md", "to": "new/path.md" }],
  "updated_index": "<full INDEX.md content>"
}
```

The parser strips markdown fences if the model emits them, then validates that `target_path` and `updated_index` are non-empty.

### Query engine prompt contract

Two sequential LLM calls for synthesis:

**Call 1 — relevance** (also used for `mode=raw`):

User prompt contains `INDEX.md` + query. Expected response:
```json
{ "paths": ["path/a.md", "path/b.md"], "explanation": "..." }
```
Capped at 5 paths. Paths that don't exist on disk are silently filtered before use.

**Call 2 — synthesis**:

System prompt instructs the model to answer from provided documents only. User prompt concatenates file contents as `=== path ===\n<content>` blocks followed by the question.

---

## Concurrency model

```
HTTP goroutines (one per request)
    │
    ├── GET requests  → read filesystem directly, no locking
    │
    └── POST requests → acquire queue mutex → write queue.json → release
                        signal worker channel (non-blocking)

Queue worker (single goroutine)
    └── owns all git operations
        no concurrent git writes possible
```

The single worker goroutine is the sole writer to the Git repository. GET handlers read files directly from the filesystem without coordination. The only shared lock is `queue.mu`, which protects `queue.json` reads and writes between the HTTP goroutines and the worker.

---

## Knowledge repository layout

```
<repo>/
├── .gitignore          queue.json (and root binaries)
├── INDEX.md            auto-maintained; one entry per file
├── queue.json          live job queue — unversioned
└── <topic>/
    └── <subtopic>/
        └── title.md    max 3 levels deep, kebab-case
```

### INDEX.md format

```markdown
# Index

## Topic
- [Title](topic/subtopic/title.md) — one-line description

## Another Topic
- [Title](another/file.md) — one-line description
```

The LLM receives the full `INDEX.md` on every write and returns the complete updated version as part of its decision. This keeps the index accurate without requiring a separate indexing pass.

### Git history

Every stored document produces one commit:

```
store(3f2e1a4b-...): tech/go/goroutines.md
```

Refactors (file moves) within the same job are included in the same commit — the repository is never left in a partially-refactored state. The commit author is always `knowledged <knowledged@local>`.
