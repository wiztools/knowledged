# Agent Guide — knowledged

This document describes how AI agents should interact with this repository and the knowledged system.

## What this system is

knowledged is a Git-backed knowledge base managed by an HTTP server. Agents can store knowledge persistently (surviving across conversations) and retrieve it later — either as raw Markdown or as LLM-synthesized answers drawn from multiple documents.

The `kc` CLI is the intended interface for agents. The server must be running before any `kc` command will work.

## Available skill

A skill file for the `kc` CLI is at:

```
.agents/skills/knowledged/SKILL.md
```

Load it when working with this system. It documents all commands, flags, output formats, and async patterns.

## Prerequisites

```sh
# Build the CLI client
go build -o kc ./cmd/kc

# Verify the server is reachable (adjust URL if needed)
curl -s http://localhost:9090/content?path=INDEX.md
```

If the server is not running, start it:

```sh
./knowledged --repo /path/to/kb --model mistral-small3.1
```

## Storing knowledge

`kc post` is async. It returns a job ID immediately; the LLM then decides where to place the content and commits it to Git in the background.

**Fire-and-forget:**
```sh
id=$(kc post --content "..." --hint "topic")
echo "queued as $id"
```

**Block until stored (preferred when you need the path):**
```sh
kc post --content "..." --hint "topic" --wait
# prints:
# job_id : <uuid>
# status : done
# path   : category/subcategory/title.md
```

**Provide a hint.** The `--hint` flag materially improves placement accuracy — always include one when the topic is clear.

**Tags are optional** but help with future retrieval:
```sh
kc post --file notes.md --hint "architecture" --tags "adr,backend" --wait
```

## Retrieving knowledge

**Fetch a known file:**
```sh
kc get --path category/subtopic/file.md
```

**Ask a question (LLM synthesis — default):**
```sh
kc get --query "what patterns do we use for error handling?"
# answer goes to stdout; sources go to stderr
```

**Get raw matching documents without synthesis (faster):**
```sh
kc get --query "docker" --mode raw
```

**Capture just the synthesized answer:**
```sh
answer=$(kc get --query "summarize our Go conventions")
```

## Editing knowledge

`kc edit` is async and commits the replacement content through the server.
Use `--wait` when you need to confirm the edit in the same session:

```sh
kc edit --path category/subtopic/file.md --file updated.md --wait
kc edit --path category/subtopic/file.md --content "Updated notes..." --wait
```

Optional `--title` and `--description` update the matching `INDEX.md` entry.

## Checking the index

`INDEX.md` is the canonical list of everything stored. Check it to avoid storing duplicates or to find file paths for `--path` lookups:

```sh
kc get --path INDEX.md
```

## Checking job status

If you stored content without `--wait`, poll with:

```sh
kc job --id <job-id>
```

A job is terminal when `status` is `done` or `failed`. Do not assume a job is done until you have confirmed it.

## Crash recovery

If the server restarts mid-job, recovery is automatic. On startup the server scans the git log for job IDs: a job whose ID appears in a commit message is marked done; otherwise it is re-queued. No manual intervention is needed.

## Dos and Don'ts

**Do:**
- Always provide `--hint` when storing content.
- Use `--wait` when you need to reference the stored path in the same session.
- Use `kc edit --wait` when changing an existing document.
- Use `--mode raw` when you only need to read documents, not synthesize.
- Check `INDEX.md` before storing to avoid near-duplicate entries.

**Don't:**
- Directly edit files inside the `--repo` directory. All writes must go through the server so they are committed and the index is updated.
- Delete or modify files under `.knowledged/` while the server is running.
- Assume a fire-and-forget `post` has completed — check with `kc job`.

## Connecting to a non-default server

All `kc` commands accept `--server` as a global flag:

```sh
kc --server http://10.0.0.5:9000 post --content "..." --wait
kc --server http://10.0.0.5:9000 get --query "..."
```

## Output format reference

**`kc post` (no `--wait`)**
```
<job-uuid>
```

**`kc post --wait` or `kc job`**
```
job_id : <uuid>
status : done | failed
path   : <repo-relative-path>    # on success
error  : <message>               # on failure
```

**`kc get --path` or `kc get --query --mode raw`**
```
=== path/to/file.md ===
<file content>
────────────────────────────────────────────────────────────
=== path/to/other.md ===
<file content>
```

**`kc get --query` (synthesis)**
- stdout: synthesized answer (plain text)
- stderr: `sources: path/a.md, path/b.md`
