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
