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

func TestOpenAI_Complete_AuthAndPayload(t *testing.T) {
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

	o := NewOpenAI(srv.URL, "sk-test", "gpt-4.1-mini", newSilentLogger())
	out, err := o.Complete(context.Background(), "sys", "usr")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if out != "hello" {
		t.Errorf("expected hello, got %q", out)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("Authorization = %q, want Bearer sk-test", gotAuth)
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("path = %q, want /v1/chat/completions", gotPath)
	}
	if gotBody["model"] != "gpt-4.1-mini" {
		t.Errorf("model = %v, want gpt-4.1-mini", gotBody["model"])
	}
	if _, ok := gotBody["response_format"]; ok {
		t.Errorf("plain Complete should not send response_format, got: %v", gotBody)
	}
}

func TestOpenAI_CompleteStructured_SendsJSONSchema(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"{\"sections\":[\"Go\"]}"}}]}`))
	}))
	defer srv.Close()

	o := NewOpenAI(srv.URL, "sk-test", "gpt-4.1-mini", newSilentLogger())
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
}

func TestOpenAI_Complete_ReasoningEffortMapping(t *testing.T) {
	cases := []struct {
		budget int
		want   string
	}{
		{0, ""},
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

			o := NewOpenAI(srv.URL, "sk-test", "test-model", newSilentLogger())
			var opts []CallOption
			if tc.budget > 0 {
				opts = append(opts, WithReasoningBudget(tc.budget))
			}
			if _, err := o.Complete(context.Background(), "s", "u", opts...); err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if tc.want == "" {
				if _, present := got["reasoning_effort"]; present {
					t.Errorf("reasoning_effort must be omitted when budget=0, got %v", got["reasoning_effort"])
				}
				return
			}
			if got["reasoning_effort"] != tc.want {
				t.Errorf("reasoning_effort = %v, want %q", got["reasoning_effort"], tc.want)
			}
		})
	}
}

func TestOpenAI_CompleteStructured_ValidatesSchema(t *testing.T) {
	o := NewOpenAI("http://unused", "sk-test", "test-model", newSilentLogger())
	if _, err := o.CompleteStructured(context.Background(), "s", "u", Schema{Name: "x"}); err == nil {
		t.Error("expected error for nil schema")
	}
	if _, err := o.CompleteStructured(context.Background(), "s", "u", Schema{Schema: map[string]any{"type": "object"}}); err == nil {
		t.Error("expected error for missing schema name")
	}
}

func TestOpenAI_DefaultsBaseURL(t *testing.T) {
	o := NewOpenAI("", "sk-test", "gpt-4.1-mini", newSilentLogger())
	if o.baseURL != defaultOpenAIURL {
		t.Errorf("default baseURL = %q, want %q", o.baseURL, defaultOpenAIURL)
	}
}

func TestOpenAI_PropagatesAPIErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Invalid API key","type":"invalid_request_error"}}`))
	}))
	defer srv.Close()

	o := NewOpenAI(srv.URL, "sk-bad", "gpt-4.1-mini", newSilentLogger())
	_, err := o.Complete(context.Background(), "s", "u")
	if err == nil {
		t.Fatal("expected error for 401 response")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error should mention 401, got: %v", err)
	}
}
