package web

import (
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/runview"
)

// keptFacts is a version's metadata, as the state database keeps it, in the shape the run page draws a step's facts in.
func keptFacts(metadata string) []runview.Fact {
	decoded := events.DecodeFacts(metadata)

	facts := make([]runview.Fact, 0, len(decoded))
	for _, fact := range decoded {
		facts = append(facts, runview.Fact{Name: fact.Name, Value: fact.Value})
	}

	return facts
}
