package llm

import "context"

// Provider is the interface every LLM backend must implement.
type Provider interface {
	// Complete sends a system prompt and a user message and returns the
	// model's free-form text response.
	Complete(ctx context.Context, system, user string) (string, error)

	// CompleteStructured asks the model for a JSON response that conforms
	// to the given schema. The returned string is the JSON body — guaranteed
	// to be parseable as the schema by whatever native mechanism the backend
	// supports (Anthropic tool_use, Ollama `format`, OpenAI json_schema).
	// Callers can json.Unmarshal the result without fence-stripping or
	// "model returned prose" defenses.
	CompleteStructured(ctx context.Context, system, user string, schema Schema) (string, error)
}

// Schema describes a JSON Schema object plus the metadata some providers need
// (Anthropic tool_use requires a tool name; OpenAI json_schema requires a name
// and accepts a description; Ollama uses just the raw schema).
type Schema struct {
	// Name is a short identifier for the schema. Must match
	// `^[a-zA-Z0-9_-]{1,64}$` to be safe across providers.
	Name string
	// Description is shown to the model alongside the schema. Optional.
	Description string
	// Schema is a JSON Schema object — typically `{"type": "object", "properties": {...}, "required": [...]}`.
	Schema map[string]any
}
