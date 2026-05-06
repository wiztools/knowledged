---
name: knowledged
description: Store, edit, delete, and retrieve knowledge using the knowledged server via the kc CLI — post content to a Git-backed knowledge base, retrieve raw documents, query with LLM synthesis, and check async job status.
---

# knowledged (`kc`)

## Prerequisites

- `kc` must be built and on `$PATH`:
  ```sh
  go build -o kc ./cmd/kc
  ```
- A `knowledged` server must be running:
  ```sh
  knowledged --repo /path/to/kb --model mistral-small3.1 --port 9090
  ```
- Default server URL is `http://localhost:9090`. Override with the global `--server` flag.

## Global flag

```
kc --server <URL> <command> [flags]
```

All commands accept `--server` before the subcommand name to target a non-default server.

## Tips for agent usage

- `kc post`, `kc edit`, and `kc delete` are **async** — all return a job ID immediately. Use `--wait` to block until the operation completes.
- When not using `--wait`, check completion with `kc job --id <id>` before assuming the operation finished.
- `kc delete` fails (job status `failed`) if the path does not exist in the repo.
- `kc edit` fails (job status `failed`) if the path does not exist in the repo.
- `kc get --query` calls the LLM on the server side; it may take several seconds.
- `kc get --mode raw` is faster and returns verbatim document content — prefer it when you only need to retrieve, not synthesize.
- Content can be piped via stdin: `echo "..." | kc post`.
- `sources:` lines from `kc get --query` are printed to stderr; only the synthesized answer goes to stdout — safe to capture with `$()` or redirect.

## Commands

### delete — remove a file

```sh
kc delete --path <repo-relative-path> [--wait] [--timeout N]
```

| Flag | Description |
|---|---|
| `--path` | Repo-relative path of the file to delete (required) |
| `--wait` | Block until the delete job completes (default: false) |
| `--timeout` | Seconds to wait when `--wait` is set (default: 120) |

Removes the file and its INDEX.md entry in a single atomic git commit.
Returns HTTP 404 / job status `failed` if the file does not exist.

Prints the **job ID** to stdout on success (or the final result table when `--wait`).

```sh
# Delete a file, fire-and-forget
kc delete --path tech/go/goroutines.md

# Delete and wait for confirmation
kc delete --path lang/rust/ownership.md --wait

# Explicit server
kc --server http://10.0.0.5:9000 delete --path notes/old.md --wait
```

### post — store content

```sh
kc post [--content TEXT] [--file PATH] [--hint TEXT] [--tags tag1,tag2] [--wait] [--timeout N]
```

| Flag | Description |
|---|---|
| `--content` | Inline content string |
| `--file` | Read content from a file |
| `--hint` | Topic hint for the LLM organizer (improves placement accuracy) |
| `--tags` | Comma-separated tags |
| `--wait` | Block until done and print the stored path (default: false) |
| `--timeout` | Seconds to wait when `--wait` is set (default: 120) |

Content source priority: `--content` > `--file` > stdin.

Prints the **job ID** to stdout on success (or the final result table when `--wait`).

```sh
# Store inline, fire-and-forget
kc post --content "Rust ownership model: each value has a single owner." --hint "rust"

# Store a file and wait for the path
kc post --file architecture.md --hint "system design" --wait

# Pipe from another command
cat notes.md | kc post --tags "meeting,q3"

# Explicit server
kc --server http://10.0.0.5:9000 post --content "..." --wait
```

### edit — replace a file

```sh
kc edit --path <repo-relative-path> [--content TEXT] [--file PATH] [--title TEXT] [--description TEXT] [--wait] [--timeout N]
```

| Flag | Description |
|---|---|
| `--path` | Repo-relative path of the Markdown file to edit (required) |
| `--content` | Replacement content string |
| `--file` | Read replacement content from a file |
| `--title` | Optional replacement title for the INDEX.md entry |
| `--description` | Optional replacement description for the INDEX.md entry |
| `--wait` | Block until the edit job completes (default: false) |
| `--timeout` | Seconds to wait when `--wait` is set (default: 120) |

Content source priority: `--content` > `--file` > stdin.

```sh
# Replace content and wait for the commit
kc edit --path tech/go/goroutines.md --file updated.md --wait

# Replace content and update index metadata
kc edit --path tech/go/goroutines.md \
  --content "Updated notes..." \
  --title "Goroutines" \
  --description "Updated runtime concurrency notes" \
  --wait
```

### get — retrieve content

```sh
kc get --path <repo-relative-path>
kc get --query <text> [--mode raw|synthesize]
```

| Flag | Description |
|---|---|
| `--path` | Repo-relative file path; always returns raw file content |
| `--query` | Natural-language query |
| `--mode` | `synthesize` (default) or `raw` — only applies with `--query` |

**Mode behavior:**

| Invocation | What is returned |
|---|---|
| `--path` | Raw content of the single file |
| `--query` (default / `--mode synthesize`) | LLM-synthesized answer drawn from relevant documents; sources printed to stderr |
| `--query --mode raw` | Raw content of all matching documents, no synthesis |

```sh
# Fetch a known file
kc get --path tech/go/goroutines.md

# Ask a question (LLM synthesis)
kc get --query "how does Rust handle memory safety?"

# Retrieve matching docs verbatim (no LLM synthesis)
kc get --query "docker setup" --mode raw

# Capture only the answer (sources go to stderr)
answer=$(kc get --query "what is the strangler fig pattern?")
```

### job — check job status

```sh
kc job --id <job-id>
```

Prints a status table:

```
job_id : 3f2e1a...
status : done
path   : tech/go/goroutines.md
```

Status values: `queued` | `processing` | `done` | `failed`

```sh
# Post async, then poll manually
id=$(kc post --content "...")
kc job --id "$id"
```

## Async store workflow (when not using --wait)

```sh
# 1. Enqueue
id=$(kc post --file research.md --hint "distributed systems")
echo "queued as $id"

# 2. Poll until done
while true; do
  status=$(kc job --id "$id" | grep '^status' | awk '{print $3}')
  [ "$status" = "done" ] || [ "$status" = "failed" ] && break
  sleep 2
done

# 3. Get the stored path
kc job --id "$id"
```

Or simply use `--wait`:

```sh
kc post --file research.md --hint "distributed systems" --wait
```

## Output formats

**`kc post` (without --wait)**
```
<job-id>
```

**`kc post --wait` / `kc edit --wait` / `kc delete --wait` / `kc job`**
```
job_id : <uuid>
status : done | failed
path   : <repo-relative-path>     # on success
error  : <message>                # on failure
```

**`kc get --path` / `kc get --query --mode raw`**
```
=== path/to/file.md ===
<file content>
────────────────────────────────────────────────────────────
=== path/to/other.md ===
<file content>
```

**`kc get --query` (synthesis)**
```
<synthesized answer>
```
_(sources line is on stderr: `sources: path/a.md, path/b.md`)_

## Error handling

- Non-zero exit code on any error.
- Error details printed to stderr via `slog`.
- Server-side errors surface as `{"error": "..."}` JSON, which `kc` prints to stderr and exits 1.
- If `kc job` returns `status: failed`, the `error` field contains the reason.
