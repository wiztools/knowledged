# MCP Server for knowledged

**Date:** 2026-04-07
**Status:** Approved

## Overview

A Model Context Protocol (MCP) server that exposes the `knowledged` knowledge base to Claude and other MCP clients. Written in Go using the `mark3labs/mcp-go` SDK. Transport: stdio. Lives in `mcp/` as a standalone binary within the existing module.

## Structure

```
mcp/
├── main.go      # entry point — wires server, registers tools, starts stdio
├── client.go    # HTTP client for knowledged API (PostContent, GetContent, CheckJob)
└── tools.go     # MCP tool definitions and handlers
```

Built as a separate binary: `go build ./mcp`

## Configuration

| Env var          | Default                   | Purpose                      |
|------------------|---------------------------|------------------------------|
| `KNOWLEDGED_URL` | `http://localhost:9090`   | Base URL for knowledged API  |

## MCP Tools

### `post_content`

Store content in the knowledge base.

| Parameter | Type   | Required | Description                                      |
|-----------|--------|----------|--------------------------------------------------|
| `content` | string | yes      | The text to store                                |
| `hint`    | string | no       | Hint for organizing/naming the document          |
| `tags`    | string | no       | Comma-separated tags                             |
| `wait`    | bool   | no       | If true, poll until job completes before returning |

Returns: `job_id`, `status`, and `path` (if `wait=true` and job succeeded).

### `get_content`

Retrieve content from the knowledge base. Either `path` or `query` must be provided.

| Parameter | Type   | Required | Description                                      |
|-----------|--------|----------|--------------------------------------------------|
| `path`    | string | no       | Repo-relative path to a specific file            |
| `query`   | string | no       | Natural language query                           |
| `mode`    | string | no       | `"raw"` (return matching docs) or `"synthesize"` (LLM answer, default) |

Returns: raw file content, list of raw docs, or LLM-synthesized answer.

### `check_job`

Check the status of an async store job.

| Parameter | Type   | Required | Description       |
|-----------|--------|----------|-------------------|
| `job_id`  | string | yes      | Job ID to look up |

Returns: `status`, `path` (if done), `error` (if failed).

## Architecture & Data Flow

```
Claude / MCP client
       │  stdin/stdout (JSON-RPC 2.0)
       ▼
  mcp/main.go
  mcp-go SDK (stdio server)
       │
  mcp/tools.go  ──→  mcp/client.go  ──→  knowledged HTTP API
                       (net/http)          :9090
```

- **`main.go`**: reads `KNOWLEDGED_URL`, creates an `mcp-go` server, registers the three tools, calls `server.ServeStdio()`
- **`client.go`**: plain `net/http` client with three typed methods — no retries, returns errors on non-2xx responses
- **`tools.go`**: thin adapters — parse MCP tool input args, call client methods, format results as `TextContent`

## Error Handling

- HTTP errors and non-2xx responses are returned as MCP tool errors via the SDK's standard error path
- Missing required parameters are validated in `tools.go` before making any HTTP call
- No automatic retries — callers can use `check_job` to poll async jobs

## Dependencies

- `github.com/mark3labs/mcp-go` — MCP stdio transport and tool registration
- Standard library `net/http` for knowledged API calls

## Build & Usage

```bash
# Build
go build -o mcp-knowledged ./mcp

# Run (Claude Desktop / Claude Code MCP config)
KNOWLEDGED_URL=http://localhost:9090 ./mcp-knowledged
```

Claude Desktop config example:
```json
{
  "mcpServers": {
    "knowledged": {
      "command": "/path/to/mcp-knowledged",
      "env": { "KNOWLEDGED_URL": "http://localhost:9090" }
    }
  }
}
```
