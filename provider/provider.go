// Package provider resolves namespaced model refs ("anthropic/claude-haiku-4-5",
// "lmstudio/qwen3-0.6b") to ADK model.LLM implementations. The agent loop lives
// in the engine; providers only do completions, so retry/budget/journal
// semantics stay identical across providers.
package provider

import (
	"errors"
	"fmt"
	"os"
	"strings"

	genaianthropic "github.com/achetronic/adk-utils-go/genai/anthropic"
	genaiopenai "github.com/achetronic/adk-utils-go/genai/openai"
	adkmodel "google.golang.org/adk/model"
)

// Factory builds an LLM for a bare model name (the part after the prefix).
type Factory func(modelName string) (adkmodel.LLM, error)

// Registry maps provider prefixes to factories.
type Registry struct {
	factories map[string]Factory
}

// NewRegistry returns a registry with the built-in providers:
//
//	anthropic/  — Anthropic API (ANTHROPIC_API_KEY)
//	openai/     — OpenAI API (OPENAI_API_KEY, OPENAI_BASE_URL)
//	ollama/     — local Ollama (OLLAMA_BASE_URL, default http://localhost:11434/v1)
//	lmstudio/   — local LM Studio (LMSTUDIO_BASE_URL, default http://localhost:1234/v1)
//
// ollama/ and lmstudio/ are the same OpenAI-compatible client with different
// default base URLs; any OpenAI-compatible gateway works via openai/ +
// OPENAI_BASE_URL.
func NewRegistry() *Registry {
	r := &Registry{factories: map[string]Factory{}}
	r.Register("anthropic", func(name string) (adkmodel.LLM, error) {
		if os.Getenv("ANTHROPIC_API_KEY") == "" {
			return nil, errors.New("ANTHROPIC_API_KEY is not set")
		}
		return genaianthropic.New(genaianthropic.Config{ModelName: name}), nil
	})
	r.Register("openai", func(name string) (adkmodel.LLM, error) {
		return genaiopenai.New(genaiopenai.Config{
			BaseURL:   os.Getenv("OPENAI_BASE_URL"),
			ModelName: name,
		}), nil
	})
	r.Register("ollama", openAICompatible("OLLAMA_BASE_URL", "http://localhost:11434/v1"))
	r.Register("lmstudio", openAICompatible("LMSTUDIO_BASE_URL", "http://localhost:1234/v1"))
	// OpenRouter is OpenAI-compatible but needs per-request cache handling
	// (x-session-id sticky routing, cache_control for Anthropic, cached-token
	// accounting) — see openrouter.go.
	r.Register("openrouter", newOpenRouter)
	return r
}

func openAICompatible(envVar, defaultURL string) Factory {
	return func(name string) (adkmodel.LLM, error) {
		base := os.Getenv(envVar)
		if base == "" {
			base = defaultURL
		}
		return genaiopenai.New(genaiopenai.Config{
			APIKey:    "local", // local servers ignore it; the SDK requires one
			BaseURL:   base,
			ModelName: name,
		}), nil
	}
}

// Register adds or replaces a provider prefix.
func (r *Registry) Register(prefix string, f Factory) { r.factories[prefix] = f }

// Has reports whether a prefix is registered.
func (r *Registry) Has(prefix string) bool { _, ok := r.factories[prefix]; return ok }

// Resolve builds an LLM from a namespaced ref like "lmstudio/qwen3-0.6b".
func (r *Registry) Resolve(ref string) (adkmodel.LLM, error) {
	prefix, name, ok := strings.Cut(ref, "/")
	if !ok {
		return nil, fmt.Errorf("model ref %q is not provider-namespaced (want e.g. anthropic/claude-haiku-4-5)", ref)
	}
	f, ok := r.factories[prefix]
	if !ok {
		known := make([]string, 0, len(r.factories))
		for k := range r.factories {
			known = append(known, k)
		}
		return nil, fmt.Errorf("unknown provider %q in model ref %q (registered: %s)", prefix, ref, strings.Join(known, ", "))
	}
	return f(name)
}
