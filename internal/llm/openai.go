package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// defaultOpenAIURL is the public OpenAI API root. Override with --openai-url
// for compatible gateways (Azure OpenAI, LiteLLM, OpenRouter, etc.).
const defaultOpenAIURL = "https://api.openai.com"

type openaiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openaiChatRequest struct {
	Model          string                `json:"model"`
	Messages       []openaiMessage       `json:"messages"`
	ResponseFormat *openaiResponseFormat `json:"response_format,omitempty"`
	// ReasoningEffort is OpenAI's reasoning toggle ("low" | "medium" | "high").
	// Only set when WithReasoningBudget is in effect. Non-reasoning models
	// (e.g. gpt-4o-mini) ignore the field.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

type openaiResponseFormat struct {
	Type       string           `json:"type"` // "json_schema"
	JSONSchema openaiJSONSchema `json:"json_schema"`
}

type openaiJSONSchema struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Schema      map[string]any `json:"schema"`
	Strict      bool           `json:"strict,omitempty"`
}

type openaiChatResponse struct {
	Choices []struct {
		Message openaiMessage `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error,omitempty"`
}

// OpenAI implements Provider against the OpenAI /v1/chat/completions endpoint.
// Authentication is via a Bearer API key passed at construction time.
type OpenAI struct {
	baseURL string
	apiKey  string
	model   string
	client  *http.Client
	logger  *slog.Logger
}

// NewOpenAI creates an OpenAI provider.
// baseURL is the API root (default "https://api.openai.com" if empty).
// apiKey is sent as `Authorization: Bearer <key>` on every request.
func NewOpenAI(baseURL, apiKey, model string, logger *slog.Logger) *OpenAI {
	if baseURL == "" {
		baseURL = defaultOpenAIURL
	}
	return &OpenAI{
		baseURL: baseURL,
		apiKey:  apiKey,
		model:   model,
		client:  &http.Client{Timeout: 120 * time.Second},
		logger:  logger,
	}
}

func (o *OpenAI) Complete(ctx context.Context, system, user string, opts ...CallOption) (string, error) {
	return o.chat(ctx, system, user, nil, opts)
}

// CompleteStructured constrains the reply to schema.Schema via OpenAI's
// response_format = json_schema (strict) mechanism. Supported on
// gpt-4o-2024-08-06 and later, gpt-4o-mini, the gpt-4.1 family, and the
// reasoning models — older snapshots will reject the request.
func (o *OpenAI) CompleteStructured(ctx context.Context, system, user string, schema Schema, opts ...CallOption) (string, error) {
	if schema.Schema == nil {
		return "", fmt.Errorf("openai: structured call requires a non-nil Schema.Schema")
	}
	if schema.Name == "" {
		return "", fmt.Errorf("openai: structured call requires a Schema.Name")
	}
	rf := &openaiResponseFormat{
		Type: "json_schema",
		JSONSchema: openaiJSONSchema{
			Name:        schema.Name,
			Description: schema.Description,
			Schema:      schema.Schema,
			Strict:      true,
		},
	}
	return o.chat(ctx, system, user, rf, opts)
}

func (o *OpenAI) chat(ctx context.Context, system, user string, rf *openaiResponseFormat, opts []CallOption) (string, error) {
	req := openaiChatRequest{
		Model: o.model,
		Messages: []openaiMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		ResponseFormat:  rf,
		ReasoningEffort: openaiReasoningEffort(ResolveReasoningBudget(opts)),
	}

	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("marshalling request: %w", err)
	}

	url := o.baseURL + "/v1/chat/completions"
	o.logger.Debug("sending request to OpenAI",
		"url", url, "model", o.model, "structured", rf != nil)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("creating http request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+o.apiKey)

	resp, err := o.client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("calling OpenAI: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("OpenAI returned HTTP %d: %s", resp.StatusCode, string(raw))
	}

	var chatResp openaiChatResponse
	if err := json.Unmarshal(raw, &chatResp); err != nil {
		return "", fmt.Errorf("unmarshalling response: %w", err)
	}
	if chatResp.Error != nil {
		return "", fmt.Errorf("OpenAI error (%s): %s", chatResp.Error.Type, chatResp.Error.Message)
	}
	if len(chatResp.Choices) == 0 {
		return "", fmt.Errorf("OpenAI returned empty choices")
	}

	content := chatResp.Choices[0].Message.Content
	o.logger.Debug("received response from OpenAI", "content_len", len(content))
	return content, nil
}

// openaiReasoningEffort maps an opaque token budget to OpenAI's
// reasoning_effort string. Returns "" when reasoning is disabled so omitempty
// drops the field.
func openaiReasoningEffort(budget int) string {
	switch {
	case budget <= 0:
		return ""
	case budget <= 512:
		return "low"
	case budget <= 2048:
		return "medium"
	default:
		return "high"
	}
}
