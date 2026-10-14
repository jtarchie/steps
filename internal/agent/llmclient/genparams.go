package llmclient

import (
	"math"

	"google.golang.org/genai"
)

// GenParams holds the generation dials an agent configures. Unset
// fields (nil pointers, zero maxTokens, empty reasoning) are left off the
// request so the model's own defaults apply.
type GenParams struct {
	Temperature *float64
	TopP        *float64
	MaxTokens   int
	Reasoning   string // "", "low", "medium", or "high"
}

//nolint:gochecknoglobals // static, read-only lookup table
var reasoningLevels = map[string]genai.ThinkingLevel{
	"low":    genai.ThinkingLevelLow,
	"medium": genai.ThinkingLevelMedium,
	"high":   genai.ThinkingLevelHigh,
}

// ApplyTo sets the configured dials on a genai generation config.
func (p GenParams) ApplyTo(cfg *genai.GenerateContentConfig) {
	if p.Temperature != nil {
		t := float32(*p.Temperature)
		cfg.Temperature = &t
	}

	if p.TopP != nil {
		t := float32(*p.TopP)
		cfg.TopP = &t
	}

	if p.MaxTokens > 0 {
		tokens := min(p.MaxTokens, math.MaxInt32)
		cfg.MaxOutputTokens = int32(tokens) //nolint:gosec,nolintlint // bounded by the min above; nolintlint because only newer gosec builds flag it
	}

	if level, ok := reasoningLevels[p.Reasoning]; ok {
		cfg.ThinkingConfig = &genai.ThinkingConfig{ThinkingLevel: level}
	}
}
