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

// anthropicURL is the Messages API endpoint. Declared as a var so tests can
// redirect to an httptest server.
var anthropicURL = "https://api.anthropic.com/v1/messages"

const anthropicVersion = "2023-06-01"
const anthropicMaxTokens = 4096

// anthropicMinThinkingBudget is the Messages API's minimum extended-thinking
// budget. Smaller positive values are clamped up rather than rejected so a
// caller asking for "any thinking at all" still gets through.
const anthropicMinThinkingBudget = 1024

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    string             `json:"system"`
	Messages  []anthropicMessage `json:"messages"`
	// Tools and ToolChoice are only set for structured-output calls.
	Tools      []anthropicTool      `json:"tools,omitempty"`
	ToolChoice *anthropicToolChoice `json:"tool_choice,omitempty"`
	// Thinking is only set when WithReasoningBudget is in effect.
	Thinking *anthropicThinking `json:"thinking,omitempty"`
}

type anthropicThinking struct {
	Type         string `json:"type"` // "enabled"
	BudgetTokens int    `json:"budget_tokens"`
}

// anthropicTool follows the Messages API tool definition shape. We use it to
// force a structured JSON response: the input_schema is the JSON Schema we
// want the model's reply to satisfy, and tool_choice locks in this tool.
type anthropicTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
}

type anthropicToolChoice struct {
	Type string `json:"type"` // "tool"
	Name string `json:"name"`
}

type anthropicContentBlock struct {
	Type string `json:"type"`
	// Text is set for type="text" blocks.
	Text string `json:"text,omitempty"`
	// Input is set for type="tool_use" blocks — it's the JSON object the
	// model produced, conforming to the tool's input_schema.
	Input json.RawMessage `json:"input,omitempty"`
	Name  string          `json:"name,omitempty"`
}

type anthropicResponse struct {
	Content []anthropicContentBlock `json:"content"`
	Error   *anthropicError         `json:"error,omitempty"`
}

type anthropicError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// Anthropic implements Provider using the Anthropic Messages API.
type Anthropic struct {
	apiKey string
	model  string
	client *http.Client
	logger *slog.Logger
}

// NewAnthropic creates an Anthropic provider.
// apiKey must be a valid Anthropic API key (sk-ant-...).
func NewAnthropic(apiKey, model string, logger *slog.Logger) *Anthropic {
	return &Anthropic{
		apiKey: apiKey,
		model:  model,
		client: &http.Client{Timeout: 120 * time.Second},
		logger: logger,
	}
}

func (a *Anthropic) Complete(ctx context.Context, system, user string, opts ...CallOption) (string, error) {
	req := anthropicRequest{
		Model:     a.model,
		MaxTokens: anthropicMaxTokens,
		System:    system,
		Messages:  []anthropicMessage{{Role: "user", Content: user}},
	}
	a.applyOptions(&req, opts)
	resp, err := a.send(ctx, req)
	if err != nil {
		return "", err
	}
	for _, block := range resp.Content {
		if block.Type == "text" {
			return block.Text, nil
		}
	}
	return "", fmt.Errorf("Anthropic API returned no text block")
}

// CompleteStructured uses Anthropic tool_use to force a JSON reply matching
// schema.Schema. The schema becomes the (single) tool's input_schema, and
// tool_choice locks the model into calling it.
func (a *Anthropic) CompleteStructured(ctx context.Context, system, user string, schema Schema, opts ...CallOption) (string, error) {
	if schema.Schema == nil {
		return "", fmt.Errorf("anthropic: structured call requires a non-nil Schema.Schema")
	}
	if schema.Name == "" {
		return "", fmt.Errorf("anthropic: structured call requires a Schema.Name")
	}
	req := anthropicRequest{
		Model:     a.model,
		MaxTokens: anthropicMaxTokens,
		System:    system,
		Messages:  []anthropicMessage{{Role: "user", Content: user}},
		Tools: []anthropicTool{{
			Name:        schema.Name,
			Description: schema.Description,
			InputSchema: schema.Schema,
		}},
		ToolChoice: &anthropicToolChoice{Type: "tool", Name: schema.Name},
	}
	a.applyOptions(&req, opts)
	resp, err := a.send(ctx, req)
	if err != nil {
		return "", err
	}
	for _, block := range resp.Content {
		if block.Type == "tool_use" && block.Name == schema.Name {
			return string(block.Input), nil
		}
	}
	return "", fmt.Errorf("Anthropic API returned no tool_use block for %q", schema.Name)
}

// applyOptions translates the generic CallOption set into Messages API
// request fields. Currently only the reasoning budget is honored.
//
// Special case: the Messages API rejects extended thinking when tool_choice
// forces a specific tool ("Thinking may not be enabled when tool_choice
// forces tool use."). CompleteStructured always forces tool_use to guarantee
// the structured-output shape, so on that path we silently skip thinking
// rather than 400 the caller. The structured guarantee wins; callers that
// want thinking on Anthropic specifically should use plain Complete with a
// JSON-shaped prompt instead.
func (a *Anthropic) applyOptions(req *anthropicRequest, opts []CallOption) {
	budget := ResolveReasoningBudget(opts)
	if budget <= 0 {
		return
	}
	if req.ToolChoice != nil {
		a.logger.Debug("anthropic: reasoning skipped — extended thinking is incompatible with forced tool_use",
			"requested_budget", budget)
		return
	}
	if budget < anthropicMinThinkingBudget {
		budget = anthropicMinThinkingBudget
	}
	req.Thinking = &anthropicThinking{Type: "enabled", BudgetTokens: budget}
	// max_tokens must exceed budget_tokens; reserve the original cap as
	// post-thinking output budget so explanations don't get clipped.
	if req.MaxTokens <= budget {
		req.MaxTokens = budget + anthropicMaxTokens
	}
}

func (a *Anthropic) send(ctx context.Context, req anthropicRequest) (*anthropicResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshalling request: %w", err)
	}

	a.logger.Debug("sending request to Anthropic",
		"model", a.model, "structured", req.ToolChoice != nil)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, anthropicURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating http request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", a.apiKey)
	httpReq.Header.Set("anthropic-version", anthropicVersion)

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("calling Anthropic API: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Anthropic API returned HTTP %d: %s", resp.StatusCode, string(raw))
	}

	var apiResp anthropicResponse
	if err := json.Unmarshal(raw, &apiResp); err != nil {
		return nil, fmt.Errorf("unmarshalling response: %w", err)
	}
	if apiResp.Error != nil {
		return nil, fmt.Errorf("Anthropic API error (%s): %s", apiResp.Error.Type, apiResp.Error.Message)
	}
	if len(apiResp.Content) == 0 {
		return nil, fmt.Errorf("Anthropic API returned empty content")
	}
	a.logger.Debug("received response from Anthropic", "blocks", len(apiResp.Content))
	return &apiResp, nil
}
