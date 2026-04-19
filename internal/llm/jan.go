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

type janMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type janChatRequest struct {
	Model    string       `json:"model"`
	Messages []janMessage `json:"messages"`
}

type janChatResponse struct {
	Choices []struct {
		Message janMessage `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Jan implements Provider using Jan AI's OpenAI-compatible /v1/chat/completions endpoint.
type Jan struct {
	baseURL string
	model   string
	client  *http.Client
	logger  *slog.Logger
}

// NewJan creates a Jan provider.
// baseURL is the Jan server root (e.g. "http://localhost:8080").
func NewJan(baseURL, model string, logger *slog.Logger) *Jan {
	return &Jan{
		baseURL: baseURL,
		model:   model,
		client:  &http.Client{Timeout: 120 * time.Second},
		logger:  logger,
	}
}

func (j *Jan) Complete(ctx context.Context, system, user string) (string, error) {
	req := janChatRequest{
		Model: j.model,
		Messages: []janMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
	}

	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("marshalling request: %w", err)
	}

	url := j.baseURL + "/v1/chat/completions"
	j.logger.Debug("sending request to Jan", "url", url, "model", j.model)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("creating http request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := j.client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("calling Jan: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Jan returned HTTP %d: %s", resp.StatusCode, string(raw))
	}

	var chatResp janChatResponse
	if err := json.Unmarshal(raw, &chatResp); err != nil {
		return "", fmt.Errorf("unmarshalling response: %w", err)
	}
	if chatResp.Error != nil {
		return "", fmt.Errorf("Jan error: %s", chatResp.Error.Message)
	}
	if len(chatResp.Choices) == 0 {
		return "", fmt.Errorf("Jan returned empty choices")
	}

	content := chatResp.Choices[0].Message.Content
	j.logger.Debug("received response from Jan", "content_len", len(content))
	return content, nil
}
