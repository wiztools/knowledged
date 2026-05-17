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

func TestJan_Complete_NoResponseFormat(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hello"}}]}`))
	}))
	defer srv.Close()

	j := NewJan(srv.URL, "test-model", newSilentLogger())
	out, err := j.Complete(context.Background(), "sys", "usr")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if out != "hello" {
		t.Errorf("expected hello, got %q", out)
	}
	if _, ok := got["response_format"]; ok {
		t.Errorf("plain Complete should not send response_format, got: %v", got)
	}
}

func TestJan_CompleteStructured_SendsJSONSchema(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"{\"sections\":[\"Go\"]}"}}]}`))
	}))
	defer srv.Close()

	j := NewJan(srv.URL, "test-model", newSilentLogger())
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
	out, err := j.CompleteStructured(context.Background(), "sys", "usr", schema)
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

func TestJan_Complete_ReasoningEffortMapping(t *testing.T) {
	cases := []struct {
		budget int
		want   string
	}{
		{0, ""},     // disabled → field absent
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

			j := NewJan(srv.URL, "test-model", newSilentLogger())
			var opts []CallOption
			if tc.budget > 0 {
				opts = append(opts, WithReasoningBudget(tc.budget))
			}
			if _, err := j.Complete(context.Background(), "s", "u", opts...); err != nil {
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

func TestJan_CompleteStructured_ValidatesSchema(t *testing.T) {
	j := NewJan("http://unused", "test-model", newSilentLogger())
	if _, err := j.CompleteStructured(context.Background(), "s", "u", Schema{Name: "x"}); err == nil {
		t.Error("expected error for nil schema")
	}
	if _, err := j.CompleteStructured(context.Background(), "s", "u", Schema{Schema: map[string]any{"type": "object"}}); err == nil {
		t.Error("expected error for missing schema name")
	}
}
