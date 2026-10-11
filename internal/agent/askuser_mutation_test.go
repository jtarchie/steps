package agent

import (
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

func TestResponderRequestListsOptionsOnlyWhenThereAreSome(t *testing.T) {
	t.Parallel()

	open := responderRequest(store.Question{AgentName: "coder", JobName: "build", Question: "Which branch?"})
	if strings.Contains(open, "The answers it expects") {
		t.Errorf("a question with no options told the responder to expect a list of answers:\n%s", open)
	}

	closed := responderRequest(store.Question{AgentName: "coder", JobName: "build", Question: "Which branch?", Options: []string{"main", "dev"}})
	if !strings.Contains(closed, "The answers it expects: main, dev") {
		t.Errorf("the responder was not shown the options:\n%s", closed)
	}
}
