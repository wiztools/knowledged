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

// defaultNousURL is the Nous Research portal root. It speaks the OpenRouter
// flavour of the OpenAI chat API (Bearer auth, /v1/chat/completions), so the
// wire format is OpenAI-compatible except for the reasoning control, which
// uses a `reasoning` object instead of OpenAI's `reasoning_effort` string.
const defaultNousURL = "https://inference-api.nousresearch.com"

// defaultSynthesisEffort is the reasoning effort applied to free-form Complete
// calls (the /answer synthesis path) when the caller doesn't specify a budget.
// GLM-class models on the portal reason by default; we keep that on for
// synthesis because it measurably improves grounded answers, but turn it off
// for structured routing/search calls where the JSON schema already does the
// work. See nousReasoningParam.
const defaultSynthesisEffort = "medium"

type nousMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type nousChatRequest struct {
	Model          string              `json:"model"`
	Messages       []nousMessage       `json:"messages"`
	ResponseFormat *nousResponseFormat `json:"response_format,omitempty"`
	// Reasoning controls the model's chain-of-thought. Unlike OpenAI's flat
	// `reasoning_effort` string, the portal takes an object: {"effort": "..."}
	// to enable thinking at a given depth, or {"enabled": false} to disable it.
	Reasoning *nousReasoning `json:"reasoning,omitempty"`
}

// nousReasoning is the portal's reasoning control. Exactly one of Effort or
// Enabled is meaningful per request: set Effort to turn reasoning on at a
// depth, or set Enabled=false to turn it off. Both fields are omitempty so the
// unused one drops out of the payload.
type nousReasoning struct {
	Effort  string `json:"effort,omitempty"`
	Enabled *bool  `json:"enabled,omitempty"`
}

type nousResponseFormat struct {
	Type       string         `json:"type"` // "json_schema"
	JSONSchema nousJSONSchema `json:"json_schema"`
}

type nousJSONSchema struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Schema      map[string]any `json:"schema"`
	Strict      bool           `json:"strict,omitempty"`
}

type nousChatResponse struct {
	Choices []struct {
		Message nousMessage `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error,omitempty"`
}

// Nous implements Provider against the Nous Research portal's
// /v1/chat/completions endpoint. Authentication is via a Bearer API key passed
// at construction time. The assistant reply is read from
// choices[0].message.content; the separate `reasoning` field the portal
// returns is intentionally ignored.
type Nous struct {
	baseURL string
	apiKey  string
	model   string
	client  *http.Client
	logger  *slog.Logger
}

// NewNous creates a Nous portal provider.
// baseURL is the API root (default defaultNousURL if empty).
// apiKey is sent as `Authorization: Bearer <key>` on every request.
func NewNous(baseURL, apiKey, model string, logger *slog.Logger) *Nous {
	if baseURL == "" {
		baseURL = defaultNousURL
	}
	return &Nous{
		baseURL: baseURL,
		apiKey:  apiKey,
		model:   model,
		client:  &http.Client{Timeout: 120 * time.Second},
		logger:  logger,
	}
}

// Complete sends a free-form request. Reasoning defaults ON (synthesis
// benefits from it); pass WithReasoningBudget to set the depth explicitly.
func (n *Nous) Complete(ctx context.Context, system, user string, opts ...CallOption) (string, error) {
	return n.chat(ctx, system, user, nil, opts)
}

// CompleteStructured constrains the reply to schema.Schema via the
// OpenAI-compatible response_format = json_schema (strict) mechanism. Reasoning
// defaults OFF — the schema enforces the shape, so the routing/search calls
// that dominate this path stay cheap and fast. Pass WithReasoningBudget to
// re-enable it (the /ask path does this, since it also synthesises an answer).
func (n *Nous) CompleteStructured(ctx context.Context, system, user string, schema Schema, opts ...CallOption) (string, error) {
	if schema.Schema == nil {
		return "", fmt.Errorf("nous: structured call requires a non-nil Schema.Schema")
	}
	if schema.Name == "" {
		return "", fmt.Errorf("nous: structured call requires a Schema.Name")
	}
	rf := &nousResponseFormat{
		Type: "json_schema",
		JSONSchema: nousJSONSchema{
			Name:        schema.Name,
			Description: schema.Description,
			Schema:      schema.Schema,
			Strict:      true,
		},
	}
	return n.chat(ctx, system, user, rf, opts)
}

func (n *Nous) chat(ctx context.Context, system, user string, rf *nousResponseFormat, opts []CallOption) (string, error) {
	req := nousChatRequest{
		Model: n.model,
		Messages: []nousMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		ResponseFormat: rf,
		Reasoning:      nousReasoningParam(ResolveReasoningBudget(opts), rf != nil),
	}

	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("marshalling request: %w", err)
	}

	url := n.baseURL + "/v1/chat/completions"
	n.logger.Debug("sending request to Nous portal",
		"url", url, "model", n.model, "structured", rf != nil)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("creating http request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+n.apiKey)

	resp, err := n.client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("calling Nous portal: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Nous portal returned HTTP %d: %s", resp.StatusCode, string(raw))
	}

	var chatResp nousChatResponse
	if err := json.Unmarshal(raw, &chatResp); err != nil {
		return "", fmt.Errorf("unmarshalling response: %w", err)
	}
	if chatResp.Error != nil {
		return "", fmt.Errorf("Nous portal error (%s): %s", chatResp.Error.Type, chatResp.Error.Message)
	}
	if len(chatResp.Choices) == 0 {
		return "", fmt.Errorf("Nous portal returned empty choices")
	}

	content := chatResp.Choices[0].Message.Content
	n.logger.Debug("received response from Nous portal", "content_len", len(content))
	return content, nil
}

// nousReasoningParam resolves the reasoning control for a single call.
//
//   - An explicit budget (WithReasoningBudget) always wins: it maps to an
//     effort string the same way the OpenAI/Jan providers map reasoning_effort.
//   - With no budget, structured calls disable reasoning (the schema does the
//     work) and free-form calls enable it at defaultSynthesisEffort.
func nousReasoningParam(budget int, structured bool) *nousReasoning {
	if budget > 0 {
		return &nousReasoning{Effort: nousReasoningEffort(budget)}
	}
	if structured {
		off := false
		return &nousReasoning{Enabled: &off}
	}
	return &nousReasoning{Effort: defaultSynthesisEffort}
}

// nousReasoningEffort maps an opaque token budget to the portal's effort
// string, using the same thresholds as the OpenAI and Jan providers.
func nousReasoningEffort(budget int) string {
	switch {
	case budget <= 512:
		return "low"
	case budget <= 2048:
		return "medium"
	default:
		return "high"
	}
}
