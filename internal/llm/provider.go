package llm

import "context"

// Provider is the interface every LLM backend must implement.
// Complete sends a system prompt and a user message and returns the
// model's text response.
type Provider interface {
	Complete(ctx context.Context, system, user string) (string, error)
}
