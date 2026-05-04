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

type ollamaMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ollamaChatRequest struct {
	Model    string          `json:"model"`
	Messages []ollamaMessage `json:"messages"`
	Stream   bool            `json:"stream"`
	// Format is the structured-output schema. Ollama 0.5+ accepts a JSON
	// Schema object here and constrains generation to match it. Omitted
	// (zero value → omitempty drops it) for free-form Complete calls.
	Format map[string]any `json:"format,omitempty"`
}

type ollamaChatResponse struct {
	Message ollamaMessage `json:"message"`
	Done    bool          `json:"done"`
	Error   string        `json:"error,omitempty"`
}

// Ollama implements Provider using the Ollama /api/chat endpoint.
type Ollama struct {
	baseURL string
	model   string
	client  *http.Client
	logger  *slog.Logger
}

// NewOllama creates an Ollama provider.
// baseURL is the Ollama server root (e.g. "http://localhost:11434").
func NewOllama(baseURL, model string, logger *slog.Logger) *Ollama {
	return &Ollama{
		baseURL: baseURL,
		model:   model,
		client:  &http.Client{Timeout: 120 * time.Second},
		logger:  logger,
	}
}

func (o *Ollama) Complete(ctx context.Context, system, user string) (string, error) {
	return o.chat(ctx, system, user, nil)
}

// CompleteStructured asks Ollama to constrain its reply to schema.Schema by
// passing it through the chat API's `format` field. The returned string is
// the JSON content of the assistant message.
func (o *Ollama) CompleteStructured(ctx context.Context, system, user string, schema Schema) (string, error) {
	if schema.Schema == nil {
		return "", fmt.Errorf("ollama: structured call requires a non-nil Schema.Schema")
	}
	return o.chat(ctx, system, user, schema.Schema)
}

func (o *Ollama) chat(ctx context.Context, system, user string, format map[string]any) (string, error) {
	req := ollamaChatRequest{
		Model: o.model,
		Messages: []ollamaMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		Stream: false,
		Format: format,
	}

	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("marshalling request: %w", err)
	}

	url := o.baseURL + "/api/chat"
	o.logger.Debug("sending request to Ollama",
		"url", url, "model", o.model, "structured", format != nil)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("creating http request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := o.client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("calling Ollama: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Ollama returned HTTP %d: %s", resp.StatusCode, string(raw))
	}

	var chatResp ollamaChatResponse
	if err := json.Unmarshal(raw, &chatResp); err != nil {
		return "", fmt.Errorf("unmarshalling response: %w", err)
	}
	if chatResp.Error != "" {
		return "", fmt.Errorf("Ollama error: %s", chatResp.Error)
	}

	o.logger.Debug("received response from Ollama", "done", chatResp.Done,
		"content_len", len(chatResp.Message.Content))

	return chatResp.Message.Content, nil
}
