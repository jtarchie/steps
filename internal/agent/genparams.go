package agent

import (
	"math"

	"google.golang.org/genai"
)

// agentGenParams holds the generation dials an agent configures. Unset
// fields (nil pointers, zero maxTokens, empty reasoning) are left off the
// request so the model's own defaults apply.
type agentGenParams struct {
	temperature *float64
	topP        *float64
	maxTokens   int
	reasoning   string // "", "low", "medium", or "high"
}

//nolint:gochecknoglobals // static, read-only lookup table
var reasoningLevels = map[string]genai.ThinkingLevel{
	"low":    genai.ThinkingLevelLow,
	"medium": genai.ThinkingLevelMedium,
	"high":   genai.ThinkingLevelHigh,
}

// applyTo sets the configured dials on a genai generation config.
func (p agentGenParams) applyTo(cfg *genai.GenerateContentConfig) {
	if p.temperature != nil {
		t := float32(*p.temperature)
		cfg.Temperature = &t
	}

	if p.topP != nil {
		t := float32(*p.topP)
		cfg.TopP = &t
	}

	if p.maxTokens > 0 {
		tokens := min(p.maxTokens, math.MaxInt32)
		cfg.MaxOutputTokens = int32(tokens) //nolint:gosec,nolintlint // bounded by the min above; nolintlint because only newer gosec builds flag it
	}

	if level, ok := reasoningLevels[p.reasoning]; ok {
		cfg.ThinkingConfig = &genai.ThinkingConfig{ThinkingLevel: level}
	}
}
