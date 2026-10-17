package greenfield

import "censi/harness/internal/llm"

// Provider-reported usage; reasoning is included in output_tokens, never added twice.
type RunUsage struct {
	InputTokens       int `json:"input_tokens"`
	CachedInputTokens int `json:"cached_input_tokens"`
	OutputTokens      int `json:"output_tokens"`
	ReasoningTokens   int `json:"reasoning_tokens"`
	Calls             int `json:"calls"`
}

func (u *RunUsage) Add(v llm.Usage) {
	u.InputTokens += v.BilledInputTokens()
	u.CachedInputTokens += v.CacheReadTokens
	u.OutputTokens += v.OutputTokens
	u.ReasoningTokens += v.ReasoningTokens
	u.Calls++
}
