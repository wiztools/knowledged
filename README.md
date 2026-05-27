# knowledged

A self-organizing, Git-backed knowledge base with an HTTP interface and LLM-powered storage and retrieval.

You write content in; the LLM decides where it belongs, keeps the folder structure tidy, and commits everything to Git. You query content back either as raw Markdown files or as synthesized answers drawn from multiple documents.

## How it works

```
POST /content ──► .knowledged/queue.json (durable) ──► worker
                                               │
                                    LLM: where does this go?
                                    refactor if needed
                                    write file + update INDEX.md
                                               │
                                         git commit
                                    (job ID in message)

startup / timer ──► .knowledged/origin-push.json ──► push origin/<current-branch> when due

GET /content ──► LLM: which docs match?
                 read files
                 LLM: synthesize answer  (or return raw)
```

Every write is a real Git commit. Crash recovery works by scanning the commit log — if a job's ID appears in a commit message, it was already completed.

## Components

| Binary | Purpose |
|---|---|
| `knowledged` | HTTP server — stores content, serves queries |
| `kc` | CLI client — `post`, `get`, `edit`, `delete`, `job`, `recent`, `ask` subcommands |

## Requirements

- Go 1.22+
- An LLM provider — one of:
  - [Ollama](https://ollama.com) running locally, with a model pulled (e.g. `ollama pull mistral-small3.1`), **or**
  - An [Anthropic API key](https://console.anthropic.com/) set in the environment, **or**
  - An [OpenAI API key](https://platform.openai.com/) set in the environment, **or**
  - A [Jan](https://jan.ai/) server running locally (OpenAI-compatible, no key)

## Build

```sh
go build -o knowledged ./cmd/knowledged
go build -o kc        ./cmd/kc
```

## Server

**Ollama (local):**
```sh
./knowledged \
  --repo        /path/to/knowledge-repo \
  --llm-provider ollama \
  --model       mistral-small3.1 \
  --port        9090
```

**Anthropic:**
```sh
export ANTHROPIC_API_KEY=sk-ant-...
./knowledged \
  --repo        /path/to/knowledge-repo \
  --llm-provider anthropic \
  --model       claude-sonnet-4-6 \
  --port        9090
```

> The `ANTHROPIC_API_KEY` environment variable is the only supported way to supply the key.
> It is never logged or written to disk.

**OpenAI:**
```sh
export OPENAI_API_KEY=sk-...
./knowledged \
  --repo        /path/to/knowledge-repo \
  --llm-provider openai \
  --model       gpt-5.5 \
  --port        9090
```

> The `OPENAI_API_KEY` environment variable is the only supported way to supply the key.
> It is never logged or written to disk.
> Use `--openai-url` to target Azure OpenAI or an OpenAI-compatible gateway (LiteLLM, OpenRouter, etc.).
> Structured output (used by `POST /ask` and the organizer) requires a model that supports
> `response_format = json_schema` with `strict: true` — `gpt-4.1-mini`, `gpt-4o-mini`,
> `gpt-4o-2024-08-06` and later, or any reasoning model.

**`--repo` behavior:**

| Directory state | Action |
|---|---|
| Does not exist | Created + `git init` |
| Exists, empty | `git init` |
| Exists, is a Git repo | Opened as-is |
| Exists, not empty, not a Git repo | **Error** |

On first init the server creates `.gitignore` (excludes `/.knowledged/`) and an empty `INDEX.md`, then makes an initial commit.

### Server flags

| Flag | Default | Description |
|---|---|---|
| `--repo` | *(required)* | Path to the knowledge Git repository |
| `--llm-provider` | `ollama` | LLM backend: `ollama`, `anthropic`, `openai`, or `jan` |
| `--model` | provider-specific | Model name. Defaults: `mistral-small3.1` (ollama), `claude-sonnet-4-6` (anthropic), `gpt-5.5` (openai), `<server-configured>` (jan) |
| `--ollama-url` | `http://localhost:11434` | Ollama server URL (Ollama provider only) |
| `--openai-url` | `https://api.openai.com` | OpenAI API base URL — override for Azure OpenAI or OpenAI-compatible gateways |
| `--jan-url` | `http://localhost:8080` | Jan server URL (Jan provider only) |
| `--port` | `9090` | HTTP listen port |
| `--push-origin-every` | `0` | If greater than zero, push the current branch to `origin` on that cadence using persisted state in `.knowledged/` |
| `--ask-reasoning-budget` | `2000` | Thinking-token budget for `POST /ask`. Enables provider-native chain-of-thought on supporting models; pass `0` to disable |

**Environment variables:**

| Variable | Required for |
|---|---|
| `ANTHROPIC_API_KEY` | `--llm-provider anthropic` |
| `OPENAI_API_KEY` | `--llm-provider openai` |

## CLI client (`kc`)

```
kc [--server URL] <command> [flags]
```

### `kc post` — store content

Content is read from `--content`, `--file`, or stdin (in that priority order).
When posting Markdown, do not include a top-level `# Title` line if it
duplicates the document title. knowledged stores the title separately in YAML
frontmatter; start the body with the first paragraph or with `##` section
headings.

```sh
# Inline, fire-and-forget — prints job ID
kc post --content "Go uses goroutines for concurrency." --hint "golang"

# From a file, block until stored — prints final path
kc post --file architecture.md --hint "system design" --wait

# Pipe from another command
cat meeting-notes.md | kc post --tags "meeting,q3"
```

| Flag | Default | Description |
|---|---|---|
| `--content` | | Inline content string |
| `--file` | | Path to file to store |
| `--hint` | | Topic hint for the LLM organizer |
| `--tags` | | Comma-separated tags |
| `--wait` | false | Block until job completes |
| `--timeout` | 120 | Seconds to wait (with `--wait`) |

### `kc get` — retrieve content

```sh
# Raw file by path
kc get --path tech/go/goroutines.md

# LLM-synthesized answer (default for --query)
kc get --query "how does Rust handle memory safety?"

# Raw matching documents, no synthesis
kc get --query "docker setup" --mode raw

# Browse by tag
kc tags
kc get --tag golang
kc get --tags "golang,concurrency" --match all
```

| Flag | Default | Description |
|---|---|---|
| `--path` | | Repo-relative file path (always raw) |
| `--query` | | Natural-language query |
| `--tag` | | Single tag to browse |
| `--tags` | | Comma-separated tags to browse |
| `--match` | `any` | Tag matching mode: `any` or `all` |
| `--mode` | `synthesize` | `raw` or `synthesize` (with `--query`) |

Synthesis: the answer goes to stdout; source file paths go to stderr — safe to capture with `$()`.

### `kc edit` — edit existing content

Content is read from `--content`, `--file`, or stdin (in that priority order).
The edit is asynchronous and committed through the same queue as posts and
deletes.
When replacing Markdown content, avoid a top-level `# Title` line that
duplicates the frontmatter title.

```sh
# Replace a document from a file and wait for the commit
kc edit --path tech/go/goroutines.md --file updated.md --wait

# Replace content inline and update the INDEX.md entry metadata
kc edit \
  --path tech/go/goroutines.md \
  --content "Updated notes..." \
  --title "Goroutines" \
  --description "Updated runtime concurrency notes" \
  --wait
```

| Flag | Default | Description |
|---|---|---|
| `--path` | | Repo-relative Markdown file path to edit |
| `--content` | | Replacement content string |
| `--file` | | Read replacement content from this file |
| `--title` | | Optional replacement title for the `INDEX.md` entry |
| `--description` | | Optional replacement description for the `INDEX.md` entry |
| `--wait` | false | Block until job completes |
| `--timeout` | 120 | Seconds to wait (with `--wait`) |

### `kc job` — check job status

```sh
kc job --id <job-id>
```

```
job_id : 3f2e1a...
status : done
path   : tech/go/goroutines.md
```

Status values: `queued` | `processing` | `done` | `failed`

### `kc ask` — draft an answer from the LLM

Sends a single-turn question to the configured LLM. The Markdown answer
is printed to stdout and the suggested tags to stderr — safe to pipe
into `kc post` without contaminating the content. Nothing is stored
until you do that.
Drafted answers omit a top-level H1 because stored notes keep the title in
frontmatter; section headings, when useful, start at `##`.

```sh
kc ask --question "what are goroutines?"
# stdout: the Markdown answer
# stderr: tags: golang, concurrency

# Draft and store in one shot (review the draft first in practice)
kc ask --question "what are goroutines?" | kc post --hint golang

# Full structured response for scripting
kc --json ask --question "what are goroutines?" | jq '{answer, tags}'
```

| Flag | Default | Description |
|---|---|---|
| `--question` | | The question to ask (required) |

### Global flags

```sh
kc --server http://10.0.0.5:9000 post --content "..."

# --json applies to any subcommand and emits the raw server response
kc --json post --content "..." --wait    # → final job JSON after polling
kc --json recent | jq '.posts[].path'
```

## HTTP API

### `POST /content`

```json
// Request
{ "content": "...", "hint": "optional", "tags": ["optional"] }

// Response 202
{ "job_id": "uuid", "status": "queued" }
```

### `PUT /content`

```json
// Request
{
  "path": "tech/go/goroutines.md",
  "content": "...replacement Markdown...",
  "title": "optional INDEX title",
  "description": "optional INDEX description"
}

// Response 202
{ "job_id": "uuid", "status": "queued" }
```

### `GET /jobs/{id}`

```json
{ "job_id": "uuid", "status": "done", "path": "tech/go/goroutines.md" }
{ "job_id": "uuid", "status": "failed", "error": "..." }
```

### `GET /content`

| Query params | Returns |
|---|---|
| `path=tech/go/file.md` | `{ "path": "...", "content": "..." }` |
| `query=<text>` | `{ "query": "...", "sources": [...], "answer": "..." }` |
| `query=<text>&mode=raw` | `[{ "path": "...", "content": "..." }, ...]` |
| `tag=golang` | `[{ "path": "...", "title": "...", "description": "...", "tags": [...], "modified": "..." }, ...]` |
| `tags=golang,concurrency&match=all` | Documents matching every supplied tag |
| `tag=golang&mode=raw` | `[{ "path": "...", "content": "..." }, ...]` |

### `GET /tags`

Returns tags from the derived cache at `.knowledged/tag-index.json`. The cache
is rebuilt from note frontmatter when missing, malformed, version-mismatched, or
stale against the repository HEAD.

```json
{ "tags": [{ "tag": "golang", "count": 12 }] }
```

### `POST /ask`

Drafts a Markdown explanation and suggested tags from the configured LLM.
Stores nothing — intended for clients that want to prefill a
"review-and-post" form. The human is always the one who decides whether
the answer and tags become a stored document via `POST /content`.

Internally uses structured output (Anthropic tool_use / Ollama `format` /
Jan json_schema) so the `tags` and `answer` fields are guaranteed
to be present. When `--ask-reasoning-budget` is non-zero (default 2000),
the call also opts into provider-native chain-of-thought — Ollama
`think=true` or Jan `reasoning_effort` — which improves answer quality
on supporting models and is silently ignored elsewhere.

> **Anthropic note:** the Messages API rejects extended thinking when
> `tool_choice` forces a specific tool, which `CompleteStructured` does
> to guarantee the JSON shape. Reasoning is therefore silently skipped
> on the Anthropic backend for `/ask` (the structured-output guarantee
> wins). The budget still applies to other backends.

```json
// Request
{ "question": "what are goroutines?" }

// Response 200
{
  "question": "what are goroutines?",
  "answer":   "## Goroutines\n\n...",
  "tags":     ["golang", "concurrency"]
}
```

`tags` is always present and is the empty array `[]` when the model
declines to suggest any (e.g. the question is unanswerable).

## Repository layout

```
<knowledge-repo>/
├── .gitignore       # contains: /.knowledged/
├── .knowledged/
│   ├── origin-push.json   # last attempted origin push time
│   └── queue.json         # live job queue (unversioned)
├── INDEX.md         # auto-maintained index of all documents
└── <topic>/
    └── <subtopic>/
        └── file.md  # organized by the LLM, max 3 levels deep
```

`INDEX.md` is kept in sync with every commit:

```markdown
# Index

## Go
- [Goroutines](tech/go/goroutines.md) — concurrency primitives in Go

## Docker
- [Setup](devops/docker/setup.md) — installing and configuring Docker
```

## Extending LLM providers

Implement the `llm.Provider` interface:

```go
type Provider interface {
    Complete(ctx context.Context, system, user string, opts ...CallOption) (string, error)
    CompleteStructured(ctx context.Context, system, user string, schema Schema, opts ...CallOption) (string, error)
}
```

Pass your implementation to `organizer.New()` and `api.NewHandler()`. No other changes needed.

Backends are free to ignore options they don't understand. The only
option today is `llm.WithReasoningBudget(n)`, which `POST /ask` forwards
when `--ask-reasoning-budget` is non-zero — see each provider's
implementation for how it maps to the backend's native reasoning knob.

## Project layout

```
cmd/
  knowledged/main.go   server binary
  kc/main.go           CLI client
internal/
  api/handler.go       HTTP handlers
  llm/provider.go      Provider interface
  llm/ollama.go        Ollama backend
  llm/anthropic.go     Anthropic backend
  llm/openai.go        OpenAI backend
  llm/jan.go           Jan (OpenAI-compatible) backend
  store/store.go       go-git wrapper
  store/index.go       INDEX.md helpers
  organizer/           LLM placement + execution
  queue/queue.go       durable async job queue
.agents/skills/
  knowledged/SKILL.md  agent skill definition
```
