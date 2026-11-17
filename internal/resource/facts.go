package resource

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/redact"
)

type factsKey struct{}

type factSink struct {
	mu    sync.Mutex
	facts []events.Fact
}

// CollectFacts is how a caller hears the facts an in: or out: under ctx hands back; facts reads them once the stage returns. On a context with no collector a stage's facts go nowhere.
func CollectFacts(ctx context.Context) (context.Context, func() []events.Fact) {
	sink := &factSink{}

	return context.WithValue(ctx, factsKey{}, sink), func() []events.Fact {
		sink.mu.Lock()
		defer sink.mu.Unlock()

		return events.CleanFacts(sink.facts)
	}
}

// reportFacts replaces what the stage under ctx reported: a retried attempt's answer is the one that counts. Masked here, under the resource's own secrets, because a fact is kept beside its version where no run's masking reaches it — and only once cleaned, or a control character inside a token would hide it from the mask and the cleaning would then join it back whole.
func reportFacts(ctx context.Context, facts []events.Fact) {
	sink, ok := ctx.Value(factsKey{}).(*factSink)
	if !ok {
		return
	}

	secrets := redact.SecretsFrom(ctx)

	masked := make([]events.Fact, 0, len(facts))
	for _, fact := range events.CleanFacts(facts) {
		masked = append(masked, events.Fact{Name: secrets.String(fact.Name), Value: secrets.String(fact.Value)})
	}

	facts = masked

	sink.mu.Lock()
	defer sink.mu.Unlock()

	sink.facts = facts
}

// decodeFacts reads Concourse's metadata, a list of {name, value}. A value that is not a string is shown as the JSON it was; an entry that is not an object is dropped, since a fact is shown, never acted on.
func decodeFacts(raw any) []events.Fact {
	entries, ok := raw.([]any)
	if !ok {
		return nil
	}

	facts := make([]events.Fact, 0, len(entries))

	for _, entry := range entries {
		fields, ok := entry.(map[string]any)
		if !ok {
			continue
		}

		facts = append(facts, events.Fact{Name: factString(fields["name"]), Value: factString(fields["value"])})
	}

	return facts
}

func factString(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case json.Number:
		return typed.String()
	}

	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprint(value)
	}

	return string(encoded)
}

// unwrapAnswer reads Concourse's answer shape, {"version": {...}, "metadata": [...]}, out of what a script printed: an object whose only keys are those, with version itself an object. Anything else is the version as it always was.
func unwrapAnswer(answer map[string]any) (map[string]any, []events.Fact, bool) {
	inner, ok := answer["version"].(map[string]any)
	if !ok {
		return answer, nil, false
	}

	for key := range answer {
		if key != "version" && key != "metadata" {
			return answer, nil, false
		}
	}

	return inner, decodeFacts(answer["metadata"]), true
}

// answersWrapped is which backends' out: answers are read for Concourse's shape. concourse unwraps its own, strictly; github's built-ins report their facts directly.
func answersWrapped(backend config.ResourceBackend) bool {
	switch backend {
	case config.BackendShell, config.BackendMCP, config.BackendExpr:
		return true
	case config.BackendConcourse, config.BackendGitHub, config.BackendWebhook, config.BackendCron, config.BackendRegistry, config.BackendService:
	}

	return false
}

// maxInAnswerBytes bounds what an in: script's stdout is kept for: an answer is one small object, and a script that prints its log there is not answering.
const maxInAnswerBytes = 64 << 10

// reportInAnswer reports the facts of an in: whose whole stdout is Concourse's answer, carrying metadata. Any other stdout is the script's own output and says nothing.
func reportInAnswer(ctx context.Context, stdout string) {
	answer, err := decodeOutVersion([]byte(stdout))
	if err != nil {
		return
	}

	if _, has := answer["metadata"]; !has {
		return
	}

	if _, facts, wrapped := unwrapAnswer(answer); wrapped {
		reportFacts(ctx, facts)
	}
}
