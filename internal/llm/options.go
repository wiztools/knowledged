package llm

// CallOption tunes a single Complete or CompleteStructured call. Providers
// that don't understand a given option silently ignore it; that's deliberate
// — callers can request optional capabilities (like extended thinking)
// without first checking which backend is configured.
type CallOption func(*callConfig)

// callConfig is the resolved option set built up by applyOptions. It is
// kept unexported on purpose: callers describe what they want via
// `With…` constructors, and provider implementations read the resolved
// values via the package-level resolvers below.
type callConfig struct {
	reasoningBudget int
}

// WithReasoningBudget enables provider-native chain-of-thought ("reasoning"
// or "extended thinking"). The integer is the model's thinking-token budget;
// interpretation is provider-specific:
//
//   - Anthropic: sent verbatim as thinking.budget_tokens. The Messages API
//     requires a minimum of 1024 tokens; smaller positive values are
//     clamped up. Only thinking-capable models (claude-sonnet-4-6,
//     claude-opus-4-7, claude-sonnet-3-7) accept this — older models will
//     return an API error.
//   - Ollama: enables think=true on the chat request. Ollama controls
//     thinking length internally, so the integer is informational and the
//     bit is the only thing forwarded. Non-reasoning models silently
//     ignore the flag.
//   - Jan / OpenAI-compatible: forwarded as reasoning_effort — "low" for
//     budgets ≤ 512, "medium" for ≤ 2048, "high" otherwise. Models that
//     don't support reasoning ignore the field.
//
// Pass 0 (or omit the option) to disable.
func WithReasoningBudget(tokens int) CallOption {
	return func(c *callConfig) { c.reasoningBudget = tokens }
}

// ResolveReasoningBudget returns the reasoning budget set by opts, or 0 if
// none was supplied. Exported so provider implementations (and tests) can
// read the resolved value without depending on the unexported callConfig.
func ResolveReasoningBudget(opts []CallOption) int {
	return applyOptions(opts).reasoningBudget
}

func applyOptions(opts []CallOption) callConfig {
	var c callConfig
	for _, opt := range opts {
		opt(&c)
	}
	return c
}
