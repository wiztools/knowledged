package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNous_Complete_AuthAndPayload(t *testing.T) {
	var (
		gotAuth string
		gotBody map[string]any
		gotPath string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hello"}}]}`))
	}))
	defer srv.Close()

	n := NewNous(srv.URL, "sk-nous-test", "z-ai/glm-5.2", newSilentLogger())
	out, err := n.Complete(context.Background(), "sys", "usr")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if out != "hello" {
		t.Errorf("expected hello, got %q", out)
	}
	if gotAuth != "Bearer sk-nous-test" {
		t.Errorf("Authorization = %q, want Bearer sk-nous-test", gotAuth)
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("path = %q, want /v1/chat/completions", gotPath)
	}
	if gotBody["model"] != "z-ai/glm-5.2" {
		t.Errorf("model = %v, want z-ai/glm-5.2", gotBody["model"])
	}
	if _, ok := gotBody["response_format"]; ok {
		t.Errorf("plain Complete should not send response_format, got: %v", gotBody)
	}
	// Free-form Complete with no budget defaults reasoning ON at medium effort.
	reasoning, ok := gotBody["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("Complete should send a reasoning object, got: %v", gotBody)
	}
	if reasoning["effort"] != "medium" {
		t.Errorf("default Complete reasoning effort = %v, want medium", reasoning["effort"])
	}
	if _, present := reasoning["enabled"]; present {
		t.Errorf("effort-based reasoning should omit enabled, got: %v", reasoning)
	}
}

func TestNous_CompleteStructured_SendsJSONSchema_AndDisablesReasoning(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"{\"sections\":[\"Go\"]}"}}]}`))
	}))
	defer srv.Close()

	n := NewNous(srv.URL, "sk-nous-test", "z-ai/glm-5.2", newSilentLogger())
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
	out, err := n.CompleteStructured(context.Background(), "sys", "usr", schema)
	if err != nil {
		t.Fatalf("CompleteStructured: %v", err)
	}
	if !strings.Contains(out, `"sections"`) {
		t.Errorf("expected JSON content, got %q", out)
	}

	rf, ok := got["response_format"].(map[string]any)
	if !ok {
		t.Fatalf("payload missing response_format object: %v", got)
	}
	if rf["type"] != "json_schema" {
		t.Errorf("response_format.type = %v, want json_schema", rf["type"])
	}
	js, ok := rf["json_schema"].(map[string]any)
	if !ok {
		t.Fatalf("missing json_schema object: %v", rf)
	}
	if js["name"] != "route" {
		t.Errorf("json_schema.name = %v, want route", js["name"])
	}
	if js["strict"] != true {
		t.Errorf("json_schema.strict = %v, want true", js["strict"])
	}
	if _, ok := js["schema"]; !ok {
		t.Errorf("json_schema missing schema key: %v", js)
	}

	// Structured calls with no budget must disable reasoning.
	reasoning, ok := got["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("structured call should send a reasoning object, got: %v", got)
	}
	if reasoning["enabled"] != false {
		t.Errorf("structured reasoning enabled = %v, want false", reasoning["enabled"])
	}
	if _, present := reasoning["effort"]; present {
		t.Errorf("disabled reasoning should omit effort, got: %v", reasoning)
	}
}

func TestNous_ReasoningBudgetMapping(t *testing.T) {
	cases := []struct {
		budget int
		want   string
	}{
		{200, "low"},
		{1024, "medium"},
		{4000, "high"},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("budget=%d", tc.budget), func(t *testing.T) {
			var got map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(body, &got)
				_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
			}))
			defer srv.Close()

			n := NewNous(srv.URL, "sk-nous-test", "test-model", newSilentLogger())
			// An explicit budget must win even on a structured call (the /ask path).
			schema := Schema{Name: "x", Schema: map[string]any{"type": "object"}}
			if _, err := n.CompleteStructured(context.Background(), "s", "u", schema, WithReasoningBudget(tc.budget)); err != nil {
				t.Fatalf("CompleteStructured: %v", err)
			}
			reasoning, ok := got["reasoning"].(map[string]any)
			if !ok {
				t.Fatalf("missing reasoning object: %v", got)
			}
			if reasoning["effort"] != tc.want {
				t.Errorf("reasoning effort = %v, want %q", reasoning["effort"], tc.want)
			}
			if _, present := reasoning["enabled"]; present {
				t.Errorf("budgeted reasoning should omit enabled, got: %v", reasoning)
			}
		})
	}
}

func TestNous_CompleteStructured_ValidatesSchema(t *testing.T) {
	n := NewNous("http://unused", "sk-nous-test", "test-model", newSilentLogger())
	if _, err := n.CompleteStructured(context.Background(), "s", "u", Schema{Name: "x"}); err == nil {
		t.Error("expected error for nil schema")
	}
	if _, err := n.CompleteStructured(context.Background(), "s", "u", Schema{Schema: map[string]any{"type": "object"}}); err == nil {
		t.Error("expected error for missing schema name")
	}
}

func TestNous_DefaultsBaseURL(t *testing.T) {
	n := NewNous("", "sk-nous-test", "z-ai/glm-5.2", newSilentLogger())
	if n.baseURL != defaultNousURL {
		t.Errorf("default baseURL = %q, want %q", n.baseURL, defaultNousURL)
	}
}

func TestNous_PropagatesAPIErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Invalid API key","type":"invalid_request_error"}}`))
	}))
	defer srv.Close()

	n := NewNous(srv.URL, "sk-bad", "z-ai/glm-5.2", newSilentLogger())
	_, err := n.Complete(context.Background(), "s", "u")
	if err == nil {
		t.Fatal("expected error for 401 response")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error should mention 401, got: %v", err)
	}
}
