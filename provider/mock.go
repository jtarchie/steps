package provider

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"os"
	"strings"
	"sync"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"
	"gopkg.in/yaml.v3"
)

// ClassifiedError carries an error class for retry/catch matching.
type ClassifiedError struct {
	Class string
	Msg   string
}

func (e *ClassifiedError) Error() string {
	if e.Msg == "" {
		return e.Class
	}
	return fmt.Sprintf("%s: %s", e.Class, e.Msg)
}

// Classify maps any error to an error class. ClassifiedErrors keep their
// class; everything else is sniffed (rate limits, timeouts) or falls back to
// provider_error.
func Classify(err error) string {
	var ce *ClassifiedError
	if errors.As(err, &ce) {
		return ce.Class
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "429"), strings.Contains(s, "rate limit"), strings.Contains(s, "rate_limit"):
		return "rate_limited"
	case strings.Contains(s, "timeout"), strings.Contains(s, "deadline"):
		return "timeout"
	}
	return "provider_error"
}

// ScriptResponse is one queued mock response: text, or an error class to raise.
type ScriptResponse struct {
	Text  string `yaml:"text"`
	Error string `yaml:"error"`
}

// Script is scripted responses keyed by state name. Each state's queue is
// consumed in order across attempts and visits — deterministic CI runs.
type Script map[string][]ScriptResponse

// LoadScript reads a mock_responses.yaml file.
func LoadScript(path string) (Script, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading mock script %s: %w", path, err)
	}
	var s Script
	err = yaml.Unmarshal(raw, &s)
	if err != nil {
		return nil, fmt.Errorf("parsing mock script %s: %w", path, err)
	}
	return s, nil
}

// Mock plays a Script. One Mock per run; ForState binds a state's queue.
type Mock struct {
	mu     sync.Mutex
	queues map[string][]ScriptResponse
}

// NewMock builds a mock provider for one run.
func NewMock(s Script) *Mock {
	queues := make(map[string][]ScriptResponse, len(s))
	for k, v := range s {
		queues[k] = append([]ScriptResponse(nil), v...)
	}
	return &Mock{queues: queues}
}

// ForState returns an LLM that pops from the state's queue.
func (m *Mock) ForState(state string) adkmodel.LLM {
	return &mockLLM{mock: m, state: state}
}

func (m *Mock) pop(state string) (ScriptResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	q := m.queues[state]
	if len(q) == 0 {
		return ScriptResponse{}, fmt.Errorf("mock script has no responses left for state %q", state)
	}
	m.queues[state] = q[1:]
	return q[0], nil
}

type mockLLM struct {
	mock  *Mock
	state string
}

func (l *mockLLM) Name() string { return "mock/" + l.state }

func (l *mockLLM) GenerateContent(ctx context.Context, req *adkmodel.LLMRequest, stream bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		resp, err := l.mock.pop(l.state)
		if err != nil {
			yield(nil, err)
			return
		}
		if resp.Error != "" {
			yield(nil, &ClassifiedError{Class: resp.Error, Msg: "injected by mock script"})
			return
		}
		// Crude token accounting so budgets and logs have signal in CI.
		promptChars := 0
		for _, c := range req.Contents {
			for _, p := range c.Parts {
				promptChars += len(p.Text)
			}
		}
		yield(&adkmodel.LLMResponse{
			Content: genai.NewContentFromText(resp.Text, genai.RoleModel),
			// crude chars/4 token estimate; overflow would need a multi-GB mock prompt/response
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount:     int32(promptChars / 4),
				CandidatesTokenCount: int32(len(resp.Text) / 4),               //nolint:gosec
				TotalTokenCount:      int32(promptChars/4 + len(resp.Text)/4), //nolint:gosec
			},
			TurnComplete: true,
		}, nil)
	}
}
