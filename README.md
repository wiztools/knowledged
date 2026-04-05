# knowledged

A self-organising, Git-backed knowledge base with an HTTP interface and LLM-powered storage and retrieval.

You write content in; the LLM decides where it belongs, keeps the folder structure tidy, and commits everything to Git. You query content back either as raw Markdown files or as synthesised answers drawn from multiple documents.

## How it works

```
POST /content ──► queue.json (durable) ──► worker
                                               │
                                    LLM: where does this go?
                                    refactor if needed
                                    write file + update INDEX.md
                                               │
                                         git commit
                                    (job ID in message)

GET /content ──► LLM: which docs match?
                 read files
                 LLM: synthesise answer  (or return raw)
```

Every write is a real Git commit. Crash recovery works by scanning the commit log — if a job's ID appears in a commit message, it was already completed.

## Components

| Binary | Purpose |
|---|---|
| `knowledged` | HTTP server — stores content, serves queries |
| `kc` | CLI client — `post`, `get`, `job` subcommands |

## Requirements

- Go 1.22+
- [Ollama](https://ollama.com) running locally (or any supported LLM provider)
- A model pulled in Ollama, e.g. `ollama pull mistral-small3.1`

## Build

```sh
go build -o knowledged ./cmd/knowledged
go build -o kc        ./cmd/kc
```

## Server

```sh
./knowledged \
  --repo     /path/to/knowledge-repo \
  --model    mistral-small3.1 \
  --port     8080 \
  --ollama-url http://localhost:11434   # default, can be omitted
```

**`--repo` behavior:**

| Directory state | Action |
|---|---|
| Does not exist | Created + `git init` |
| Exists, empty | `git init` |
| Exists, is a Git repo | Opened as-is |
| Exists, not empty, not a Git repo | **Error** |

On first init the server creates `.gitignore` (excludes `queue.json`) and an empty `INDEX.md`, then makes an initial commit.

### Server flags

| Flag | Default | Description |
|---|---|---|
| `--repo` | *(required)* | Path to the knowledge Git repository |
| `--model` | `mistral-small3.1` | Ollama model name |
| `--llm-provider` | `ollama` | LLM backend (`ollama` for v1) |
| `--ollama-url` | `http://localhost:11434` | Ollama server URL |
| `--port` | `8080` | HTTP listen port |

## CLI client (`kc`)

```
kc [--server URL] <command> [flags]
```

### `kc post` — store content

Content is read from `--content`, `--file`, or stdin (in that priority order).

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

# LLM-synthesised answer (default for --query)
kc get --query "how does Rust handle memory safety?"

# Raw matching documents, no synthesis
kc get --query "docker setup" --mode raw
```

| Flag | Default | Description |
|---|---|---|
| `--path` | | Repo-relative file path (always raw) |
| `--query` | | Natural-language query |
| `--mode` | `synthesize` | `raw` or `synthesize` (with `--query`) |

Synthesis: the answer goes to stdout; source file paths go to stderr — safe to capture with `$()`.

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

### Global flag

```sh
kc --server http://10.0.0.5:9000 post --content "..."
```

## HTTP API

### `POST /content`

```json
// Request
{ "content": "...", "hint": "optional", "tags": ["optional"] }

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

## Repository layout

```
<knowledge-repo>/
├── .gitignore       # contains: queue.json
├── INDEX.md         # auto-maintained index of all documents
├── queue.json       # live job queue (unversioned)
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
    Complete(ctx context.Context, system, user string) (string, error)
}
```

Pass your implementation to `organizer.New()` and `api.NewHandler()`. No other changes needed.

## Project layout

```
cmd/
  knowledged/main.go   server binary
  kc/main.go           CLI client
internal/
  api/handler.go       HTTP handlers
  llm/provider.go      Provider interface
  llm/ollama.go        Ollama backend
  store/store.go       go-git wrapper
  store/index.go       INDEX.md helpers
  organizer/           LLM placement + execution
  queue/queue.go       durable async job queue
.agents/skills/
  knowledged/SKILL.md  agent skill definition
```
