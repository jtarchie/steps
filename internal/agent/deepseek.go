package agent

import (
	"maps"
	"strings"

	genaiopenai "github.com/achetronic/adk-utils-go/genai/openai/completions"
	"github.com/openai/openai-go/v3"
	"google.golang.org/adk/v2/model"
)

// dialectFor picks the client's provider dialect by model name rather than endpoint, since one gateway (OpenCode, OpenRouter) serves many providers; nil keeps the adapter OpenAI-pure, which is every model but DeepSeek's.
func dialectFor(modelName string) genaiopenai.Dialect {
	if strings.Contains(strings.ToLower(modelName), "deepseek") {
		return deepSeekDialect{}
	}

	return nil
}

// deepSeekDialect is the library's DeepSeek dialect, which sends the model's own reasoning back, plus an empty reasoning_content on assistant turns that have none: steps injects context_paths: and upstream decisions as synthetic tool calls no model reasoned about, and a thinking-mode upstream rejects any tool-history assistant turn missing the key.
type deepSeekDialect struct {
	genaiopenai.DeepSeekDialect
}

// AdjustParams runs after the reasoning encoder, so a turn that already carries the model's reasoning keeps it.
func (deepSeekDialect) AdjustParams(params *openai.ChatCompletionNewParams, _ *model.LLMRequest, _ bool) {
	for i := range params.Messages {
		msg := params.Messages[i].OfAssistant
		if msg == nil {
			continue
		}

		extra := msg.ExtraFields()
		if _, ok := extra["reasoning_content"]; ok {
			continue
		}

		merged := maps.Clone(extra)
		if merged == nil {
			merged = map[string]any{}
		}

		merged["reasoning_content"] = ""
		msg.SetExtraFields(merged)
	}
}
