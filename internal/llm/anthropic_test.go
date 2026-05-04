package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// withAnthropicURL temporarily redirects the package-level Anthropic API URL
// to ts. Restored on test cleanup.
func withAnthropicURL(t *testing.T, ts *httptest.Server) {
	t.Helper()
	orig := anthropicURL
	anthropicURL = ts.URL
	t.Cleanup(func() { anthropicURL = orig })
}

func TestAnthropic_Complete_ReturnsTextBlock(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"hello"}]}`))
	}))
	defer srv.Close()
	withAnthropicURL(t, srv)

	a := NewAnthropic("sk-test", "claude-test", newSilentLogger())
	out, err := a.Complete(context.Background(), "sys", "usr")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if out != "hello" {
		t.Errorf("expected hello, got %q", out)
	}
}

func TestAnthropic_CompleteStructured_SendsToolAndExtractsInput(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		_, _ = w.Write([]byte(`{"content":[{"type":"tool_use","name":"route","input":{"sections":["Go"]}}]}`))
	}))
	defer srv.Close()
	withAnthropicURL(t, srv)

	a := NewAnthropic("sk-test", "claude-test", newSilentLogger())
	schema := Schema{
		Name:        "route",
		Description: "Pick sections.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"sections": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			},
			"required": []string{"sections"},
		},
	}
	out, err := a.CompleteStructured(context.Background(), "sys", "usr", schema)
	if err != nil {
		t.Fatalf("CompleteStructured: %v", err)
	}
	if !strings.Contains(out, `"sections"`) {
		t.Errorf("expected JSON content, got %q", out)
	}

	tools, ok := got["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("expected one tool in payload, got: %v", got["tools"])
	}
	toolMap, _ := tools[0].(map[string]any)
	if toolMap["name"] != "route" {
		t.Errorf("tool name = %v, want route", toolMap["name"])
	}
	if _, ok := toolMap["input_schema"]; !ok {
		t.Errorf("tool missing input_schema, got: %v", toolMap)
	}

	tc, ok := got["tool_choice"].(map[string]any)
	if !ok || tc["type"] != "tool" || tc["name"] != "route" {
		t.Errorf("tool_choice not forced to route, got: %v", got["tool_choice"])
	}
}

func TestAnthropic_CompleteStructured_NoToolUseBlockErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"refused"}]}`))
	}))
	defer srv.Close()
	withAnthropicURL(t, srv)

	a := NewAnthropic("sk-test", "claude-test", newSilentLogger())
	schema := Schema{Name: "route", Schema: map[string]any{"type": "object"}}
	if _, err := a.CompleteStructured(context.Background(), "s", "u", schema); err == nil {
		t.Fatal("expected error when no tool_use block present")
	}
}

func TestAnthropic_CompleteStructured_ValidatesSchema(t *testing.T) {
	a := NewAnthropic("sk-test", "claude-test", newSilentLogger())
	if _, err := a.CompleteStructured(context.Background(), "s", "u", Schema{Name: "x"}); err == nil {
		t.Error("expected error for nil schema")
	}
	if _, err := a.CompleteStructured(context.Background(), "s", "u", Schema{Schema: map[string]any{"type": "object"}}); err == nil {
		t.Error("expected error for missing schema name")
	}
}
