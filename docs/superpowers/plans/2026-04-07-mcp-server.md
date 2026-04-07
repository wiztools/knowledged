# MCP Server Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a Go MCP server (`mcp/` binary) that exposes the knowledged knowledge base to Claude and other MCP clients via three tools: `post_content`, `get_content`, and `check_job`.

**Architecture:** Three files — `mcp/client.go` (HTTP client for knowledged API), `mcp/tools.go` (MCP tool definitions and handlers), `mcp/main.go` (wires everything, starts stdio server). The binary speaks the MCP stdio protocol using the `mark3labs/mcp-go` SDK.

**Tech Stack:** Go 1.22, `github.com/mark3labs/mcp-go`, stdlib `net/http`, `encoding/json`

---

## File Map

| File | Action | Purpose |
|------|--------|---------|
| `mcp/client.go` | Create | HTTP client — `PostContent`, `GetContent`, `CheckJob` methods |
| `mcp/tools.go` | Create | MCP tool definitions and handler functions |
| `mcp/main.go` | Create | Entry point — env config, server wiring, `ServeStdio()` |
| `go.mod` / `go.sum` | Modify | Add `github.com/mark3labs/mcp-go` dependency |
| `bld.sh` | Modify | Add `mcp` binary to `build` and `install` targets |

---

## Task 1: Add mcp-go dependency

**Files:**
- Modify: `go.mod`
- Modify: `go.sum`

- [ ] **Step 1: Add the dependency**

```bash
cd /Users/subhash/code/bhq/knowledged
go get github.com/mark3labs/mcp-go@latest
```

Expected output: something like `go: added github.com/mark3labs/mcp-go v0.x.x`

- [ ] **Step 2: Verify module resolves**

```bash
go list -m github.com/mark3labs/mcp-go
```

Expected: prints the module path and version with no error.

- [ ] **Step 3: Commit**

```bash
git add go.mod go.sum
git commit -m "deps: add mark3labs/mcp-go for MCP server"
```

---

## Task 2: Create `mcp/client.go` — HTTP client for knowledged

**Files:**
- Create: `mcp/client.go`

- [ ] **Step 1: Create the file**

```go
// mcp/client.go
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var httpClient = &http.Client{Timeout: 180 * time.Second}

// Client calls the knowledged HTTP API.
type Client struct {
	base string // e.g. "http://localhost:9090"
}

// NewClient creates a Client pointed at base (trailing slash stripped).
func NewClient(base string) *Client {
	return &Client{base: strings.TrimRight(base, "/")}
}

// ── request/response types ────────────────────────────────────────────────────

type postContentRequest struct {
	Content string   `json:"content"`
	Hint    string   `json:"hint,omitempty"`
	Tags    []string `json:"tags,omitempty"`
}

type postContentResponse struct {
	JobID  string `json:"job_id"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

type jobStatusResponse struct {
	JobID  string `json:"job_id"`
	Status string `json:"status"`
	Path   string `json:"path,omitempty"`
	Error  string `json:"error,omitempty"`
}

type rawDocResponse struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type synthesisResponse struct {
	Query   string   `json:"query"`
	Sources []string `json:"sources"`
	Answer  string   `json:"answer"`
	Error   string   `json:"error,omitempty"`
}

// ── methods ───────────────────────────────────────────────────────────────────

// PostContent enqueues a store job and returns the job ID and initial status.
func (c *Client) PostContent(content, hint string, tags []string) (*postContentResponse, error) {
	body := postContentRequest{Content: content, Hint: hint, Tags: tags}
	var resp postContentResponse
	if err := c.postJSON("/content", body, &resp); err != nil {
		return nil, err
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("server error: %s", resp.Error)
	}
	return &resp, nil
}

// CheckJob returns the current status of a job by ID.
func (c *Client) CheckJob(jobID string) (*jobStatusResponse, error) {
	var resp jobStatusResponse
	if err := c.getJSON("/jobs/"+jobID, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// GetRawFile retrieves a single file by repo-relative path.
func (c *Client) GetRawFile(path string) (*rawDocResponse, error) {
	params := url.Values{}
	params.Set("path", path)
	var resp rawDocResponse
	if err := c.getJSON("/content?"+params.Encode(), &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// GetRawDocs retrieves matching documents without LLM synthesis.
func (c *Client) GetRawDocs(query string) ([]rawDocResponse, error) {
	params := url.Values{}
	params.Set("query", query)
	params.Set("mode", "raw")
	var resp []rawDocResponse
	if err := c.getJSON("/content?"+params.Encode(), &resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// GetSynthesis returns an LLM-synthesized answer for a query.
func (c *Client) GetSynthesis(query string) (*synthesisResponse, error) {
	params := url.Values{}
	params.Set("query", query)
	var resp synthesisResponse
	if err := c.getJSON("/content?"+params.Encode(), &resp); err != nil {
		return nil, err
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("server error: %s", resp.Error)
	}
	return &resp, nil
}

// ── HTTP helpers ──────────────────────────────────────────────────────────────

func (c *Client) postJSON(path string, body any, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encoding request: %w", err)
	}
	resp, err := httpClient.Post(c.base+path, "application/json", bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("POST %s: %w", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, path, string(raw))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decoding response (HTTP %d): %w\nbody: %s", resp.StatusCode, err, string(raw))
	}
	return nil
}

func (c *Client) getJSON(path string, out any) error {
	resp, err := httpClient.Get(c.base + path)
	if err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, path, string(raw))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decoding response (HTTP %d): %w\nbody: %s", resp.StatusCode, err, string(raw))
	}
	return nil
}
```

- [ ] **Step 2: Verify it compiles**

```bash
cd /Users/subhash/code/bhq/knowledged
go build ./mcp/...
```

Expected: no output (compiles cleanly). If `mcp/` doesn't exist yet, this will say "no Go files" — that's fine, create the directory first with `mkdir -p mcp`.

- [ ] **Step 3: Commit**

```bash
git add mcp/client.go
git commit -m "feat(mcp): add knowledged HTTP client"
```

---

## Task 3: Create `mcp/tools.go` — MCP tool definitions and handlers

**Files:**
- Create: `mcp/tools.go`

This file defines the three MCP tools and their handler functions. It depends on `Client` from `mcp/client.go`.

- [ ] **Step 1: Create the file**

```go
// mcp/tools.go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// registerTools adds all three knowledged tools to the MCP server.
func registerTools(s *server.MCPServer, c *Client) {
	// post_content
	s.AddTool(
		mcp.NewTool("post_content",
			mcp.WithDescription("Store content in the knowledged knowledge base. Returns a job_id. Use wait=true to block until the job completes."),
			mcp.WithString("content",
				mcp.Required(),
				mcp.Description("The text content to store"),
			),
			mcp.WithString("hint",
				mcp.Description("Optional topic hint to help organize the document (e.g. 'golang', 'meeting-notes')"),
			),
			mcp.WithString("tags",
				mcp.Description("Optional comma-separated tags (e.g. 'go,concurrency')"),
			),
			mcp.WithBoolean("wait",
				mcp.Description("If true, poll until the job completes and return the stored path. Default: false."),
			),
		),
		makePostContentHandler(c),
	)

	// get_content
	s.AddTool(
		mcp.NewTool("get_content",
			mcp.WithDescription("Retrieve content from the knowledged knowledge base. Provide either 'path' (exact file) or 'query' (natural language search). With query, use mode='raw' for matching docs or mode='synthesize' (default) for an LLM answer."),
			mcp.WithString("path",
				mcp.Description("Repo-relative file path to retrieve (e.g. 'tech/go/goroutines.md')"),
			),
			mcp.WithString("query",
				mcp.Description("Natural language query to search the knowledge base"),
			),
			mcp.WithString("mode",
				mcp.Description("Response mode for query: 'synthesize' (LLM answer, default) or 'raw' (return matching documents)"),
				mcp.Enum("synthesize", "raw"),
			),
		),
		makeGetContentHandler(c),
	)

	// check_job
	s.AddTool(
		mcp.NewTool("check_job",
			mcp.WithDescription("Check the status of an async post_content job. Returns status, stored path (if done), or error message (if failed)."),
			mcp.WithString("job_id",
				mcp.Required(),
				mcp.Description("The job ID returned by post_content"),
			),
		),
		makeCheckJobHandler(c),
	)
}

// ── handler factories ─────────────────────────────────────────────────────────

func makePostContentHandler(c *Client) func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		content, err := req.RequireString("content")
		if err != nil {
			return mcp.NewToolResultError("missing required parameter: content"), nil
		}

		hint := req.GetString("hint", "")
		wait := req.GetBool("wait", false)

		var tags []string
		if raw := req.GetString("tags", ""); raw != "" {
			for _, t := range strings.Split(raw, ",") {
				if t = strings.TrimSpace(t); t != "" {
					tags = append(tags, t)
				}
			}
		}

		resp, err := c.PostContent(content, hint, tags)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("post_content failed: %s", err)), nil
		}

		if !wait {
			result := fmt.Sprintf("job_id: %s\nstatus: %s", resp.JobID, resp.Status)
			return mcp.NewToolResultText(result), nil
		}

		// Poll until done or 120s timeout.
		deadline := time.Now().Add(120 * time.Second)
		interval := 2 * time.Second
		for {
			job, err := c.CheckJob(resp.JobID)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("polling job %s: %s", resp.JobID, err)), nil
			}
			if job.Status == "done" {
				result := fmt.Sprintf("job_id: %s\nstatus: done\npath: %s", job.JobID, job.Path)
				return mcp.NewToolResultText(result), nil
			}
			if job.Status == "failed" {
				return mcp.NewToolResultError(fmt.Sprintf("job %s failed: %s", job.JobID, job.Error)), nil
			}
			if time.Now().Add(interval).After(deadline) {
				return mcp.NewToolResultError(fmt.Sprintf("timed out waiting for job %s (last status: %s)", resp.JobID, job.Status)), nil
			}
			time.Sleep(interval)
		}
	}
}

func makeGetContentHandler(c *Client) func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		path := req.GetString("path", "")
		query := req.GetString("query", "")
		mode := req.GetString("mode", "synthesize")

		if path == "" && query == "" {
			return mcp.NewToolResultError("provide either 'path' or 'query'"), nil
		}

		if path != "" {
			doc, err := c.GetRawFile(path)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("get_content failed: %s", err)), nil
			}
			return mcp.NewToolResultText(fmt.Sprintf("=== %s ===\n%s", doc.Path, doc.Content)), nil
		}

		// query path
		if mode == "raw" {
			docs, err := c.GetRawDocs(query)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("get_content failed: %s", err)), nil
			}
			if len(docs) == 0 {
				return mcp.NewToolResultText("No matching documents found."), nil
			}
			var sb strings.Builder
			for i, d := range docs {
				if i > 0 {
					sb.WriteString("\n" + strings.Repeat("─", 60) + "\n")
				}
				fmt.Fprintf(&sb, "=== %s ===\n%s", d.Path, d.Content)
			}
			return mcp.NewToolResultText(sb.String()), nil
		}

		// synthesize (default)
		synth, err := c.GetSynthesis(query)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("get_content failed: %s", err)), nil
		}
		var sb strings.Builder
		if len(synth.Sources) > 0 {
			fmt.Fprintf(&sb, "Sources: %s\n\n", strings.Join(synth.Sources, ", "))
		}
		sb.WriteString(synth.Answer)
		return mcp.NewToolResultText(sb.String()), nil
	}
}

func makeCheckJobHandler(c *Client) func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		jobID, err := req.RequireString("job_id")
		if err != nil {
			return mcp.NewToolResultError("missing required parameter: job_id"), nil
		}

		job, err := c.CheckJob(jobID)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("check_job failed: %s", err)), nil
		}

		data, _ := json.MarshalIndent(job, "", "  ")
		return mcp.NewToolResultText(string(data)), nil
	}
}
```

- [ ] **Step 2: Verify it compiles (main.go doesn't exist yet so use vet on just these files)**

```bash
cd /Users/subhash/code/bhq/knowledged
go build ./mcp/...
```

Expected: may error with "undefined: server.MCPServer" until `main.go` exists — that's OK. If you see import errors for `mcp-go`, run `go mod tidy` first.

- [ ] **Step 3: Commit**

```bash
git add mcp/tools.go
git commit -m "feat(mcp): add MCP tool definitions for post_content, get_content, check_job"
```

---

## Task 4: Create `mcp/main.go` — entry point

**Files:**
- Create: `mcp/main.go`

- [ ] **Step 1: Create the file**

```go
// mcp/main.go — MCP stdio server for knowledged
//
// Usage:
//
//	KNOWLEDGED_URL=http://localhost:9090 ./mcp-knowledged
//
// Environment variables:
//
//	KNOWLEDGED_URL   knowledged server base URL (default http://localhost:9090)
package main

import (
	"fmt"
	"os"

	"github.com/mark3labs/mcp-go/server"
)

const defaultKnowledgedURL = "http://localhost:9090"

func main() {
	baseURL := os.Getenv("KNOWLEDGED_URL")
	if baseURL == "" {
		baseURL = defaultKnowledgedURL
	}

	c := NewClient(baseURL)

	s := server.NewMCPServer(
		"knowledged",
		"1.0.0",
		server.WithToolCapabilities(false),
	)

	registerTools(s, c)

	fmt.Fprintf(os.Stderr, "knowledged MCP server starting (server: %s)\n", baseURL)
	if err := server.ServeStdio(s); err != nil {
		fmt.Fprintf(os.Stderr, "MCP server error: %v\n", err)
		os.Exit(1)
	}
}
```

- [ ] **Step 2: Verify full build**

```bash
cd /Users/subhash/code/bhq/knowledged
go build ./mcp/...
```

Expected: no output, compiles cleanly.

- [ ] **Step 3: Verify the binary runs and responds to an initialize probe**

```bash
cd /Users/subhash/code/bhq/knowledged
go build -o /tmp/mcp-knowledged ./mcp
echo '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}' \
  | /tmp/mcp-knowledged 2>/dev/null
```

Expected: a JSON response containing `"result"` with `"serverInfo":{"name":"knowledged",...}` and a `"capabilities"` field with `"tools"`.

- [ ] **Step 4: Commit**

```bash
git add mcp/main.go
git commit -m "feat(mcp): add MCP stdio entry point"
```

---

## Task 5: Update `bld.sh` to include the `mcp` binary

**Files:**
- Modify: `bld.sh`

- [ ] **Step 1: Edit bld.sh**

Replace the `build` case block:

```bash
  build)
    echo "Building knowledged..."
    go build -o knowledged ./cmd/knowledged
    echo "Building kc..."
    go build -o kc ./cmd/kc
    echo "Building mcp-knowledged..."
    go build -o mcp-knowledged ./mcp
    echo "Done."
    ;;
```

Replace the `install` case block:

```bash
  install)
    echo "Installing knowledged..."
    go install ./cmd/knowledged
    echo "Installing kc..."
    go install ./cmd/kc
    echo "Installing mcp-knowledged..."
    go install ./mcp
    echo "Done. Binaries installed to $(go env GOPATH)/bin"
    ;;
```

- [ ] **Step 2: Verify bld.sh builds all three binaries**

```bash
cd /Users/subhash/code/bhq/knowledged
bash bld.sh build
ls -la knowledged kc mcp-knowledged
```

Expected: all three files listed, all with recent timestamps.

- [ ] **Step 3: Commit**

```bash
git add bld.sh
git commit -m "build: add mcp-knowledged to bld.sh build and install targets"
```

---

## Task 6: Run `go mod tidy` and final verification

**Files:**
- Modify: `go.mod`, `go.sum` (cleanup)

- [ ] **Step 1: Tidy the module**

```bash
cd /Users/subhash/code/bhq/knowledged
go mod tidy
```

Expected: no errors. `go.sum` may gain or lose some indirect entries.

- [ ] **Step 2: Verify tools/list response**

```bash
go build -o /tmp/mcp-knowledged ./mcp
printf '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}\n{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}\n' \
  | /tmp/mcp-knowledged 2>/dev/null
```

Expected: two JSON responses. The second should contain `"tools"` with three entries: `post_content`, `get_content`, `check_job`.

- [ ] **Step 3: Commit if go.mod/go.sum changed**

```bash
git add go.mod go.sum
git diff --cached --quiet || git commit -m "deps: go mod tidy after mcp-go addition"
```

---

## Claude Desktop / Claude Code integration

After the binary is built, add it to your MCP config:

**Claude Code** (`~/.claude/settings.json` `mcpServers` section):

```json
{
  "mcpServers": {
    "knowledged": {
      "command": "/Users/subhash/go/bin/mcp-knowledged",
      "env": {
        "KNOWLEDGED_URL": "http://localhost:9090"
      }
    }
  }
}
```

**Claude Desktop** (`~/Library/Application Support/Claude/claude_desktop_config.json`):

```json
{
  "mcpServers": {
    "knowledged": {
      "command": "/Users/subhash/go/bin/mcp-knowledged",
      "env": {
        "KNOWLEDGED_URL": "http://localhost:9090"
      }
    }
  }
}
```
