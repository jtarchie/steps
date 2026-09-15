package agent

import (
	"context"
	"iter"

	genaiopenai "github.com/achetronic/adk-utils-go/genai/openai/completions"
	"google.golang.org/adk/v2/model"
)

// llmFuncs holds method values, not the model: adk-utils' Model in an interface keeps all of openai-go linked (~8MB) via its client field.
type llmFuncs struct {
	name     func() string
	generate func(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error]
}

func newLLMFuncs(m *genaiopenai.Model) llmFuncs {
	return llmFuncs{name: m.Name, generate: m.GenerateContent}
}

func (f llmFuncs) Name() string { return f.name() }

func (f llmFuncs) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return f.generate(ctx, req, stream)
}
