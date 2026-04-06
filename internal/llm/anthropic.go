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

const anthropicAPIURL = "https://api.anthropic.com/v1/messages"
const anthropicVersion = "2023-06-01"
const anthropicMaxTokens = 4096

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    string             `json:"system"`
	Messages  []anthropicMessage `json:"messages"`
}

type anthropicContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
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

func (a *Anthropic) Complete(ctx context.Context, system, user string) (string, error) {
	req := anthropicRequest{
		Model:     a.model,
		MaxTokens: anthropicMaxTokens,
		System:    system,
		Messages: []anthropicMessage{
			{Role: "user", Content: user},
		},
	}

	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("marshalling request: %w", err)
	}

	a.logger.Debug("sending request to Anthropic", "model", a.model)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, anthropicAPIURL, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("creating http request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", a.apiKey)
	httpReq.Header.Set("anthropic-version", anthropicVersion)

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("calling Anthropic API: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Anthropic API returned HTTP %d: %s", resp.StatusCode, string(raw))
	}

	var apiResp anthropicResponse
	if err := json.Unmarshal(raw, &apiResp); err != nil {
		return "", fmt.Errorf("unmarshalling response: %w", err)
	}
	if apiResp.Error != nil {
		return "", fmt.Errorf("Anthropic API error (%s): %s", apiResp.Error.Type, apiResp.Error.Message)
	}
	if len(apiResp.Content) == 0 {
		return "", fmt.Errorf("Anthropic API returned empty content")
	}

	text := apiResp.Content[0].Text
	a.logger.Debug("received response from Anthropic", "content_len", len(text))

	return text, nil
}
