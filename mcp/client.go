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
