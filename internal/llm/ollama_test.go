package llm

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func newSilentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestOllama_Complete_NoFormatField(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		_, _ = w.Write([]byte(`{"message":{"role":"assistant","content":"hello"},"done":true}`))
	}))
	defer srv.Close()

	o := NewOllama(srv.URL, "test-model", newSilentLogger())
	out, err := o.Complete(context.Background(), "sys", "usr")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if out != "hello" {
		t.Errorf("expected hello, got %q", out)
	}
	if _, ok := got["format"]; ok {
		t.Errorf("plain Complete should not send a format field, got payload: %v", got)
	}
}

func TestOllama_CompleteStructured_SendsFormat(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		_, _ = w.Write([]byte(`{"message":{"role":"assistant","content":"{\"sections\":[\"Go\"]}"},"done":true}`))
	}))
	defer srv.Close()

	o := NewOllama(srv.URL, "test-model", newSilentLogger())
	schema := Schema{
		Name: "route",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"sections": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			},
			"required": []string{"sections"},
		},
	}
	out, err := o.CompleteStructured(context.Background(), "sys", "usr", schema)
	if err != nil {
		t.Fatalf("CompleteStructured: %v", err)
	}
	if !strings.Contains(out, `"sections"`) {
		t.Errorf("expected JSON content, got %q", out)
	}

	format, ok := got["format"].(map[string]any)
	if !ok {
		t.Fatalf("payload missing format object: %v", got)
	}
	if format["type"] != "object" {
		t.Errorf("format.type = %v, want object", format["type"])
	}
	props, _ := format["properties"].(map[string]any)
	if _, ok := props["sections"]; !ok {
		t.Errorf("format.properties missing sections key: %v", format)
	}
}

func TestOllama_CompleteStructured_NilSchemaErrors(t *testing.T) {
	o := NewOllama("http://unused", "test-model", newSilentLogger())
	_, err := o.CompleteStructured(context.Background(), "s", "u", Schema{Name: "x"})
	if err == nil {
		t.Fatal("expected error for nil schema, got nil")
	}
}
