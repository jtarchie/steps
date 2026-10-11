package agent

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

// TestCheckProviderToolNameAcceptsExactlyTheLimit pins the boundary: the providers' 64 is an inclusive maximum, so a name of exactly that length is one the model can be offered.
func TestCheckProviderToolNameAcceptsExactlyTheLimit(t *testing.T) {
	t.Parallel()

	server := "linear"
	prefix := server + config.MCPToolNameSep
	remote := strings.Repeat("x", maxProviderToolName-len(prefix))

	err := checkProviderToolName(server, remote, prefix+remote)
	if err != nil {
		t.Errorf("a %d-character name: %v, want it accepted", maxProviderToolName, err)
	}

	err = checkProviderToolName(server, remote+"x", prefix+remote+"x")
	if err == nil {
		t.Errorf("a %d-character name was accepted, want it refused", maxProviderToolName+1)
	}
}

// TestBoundedStructuredContentKeepsAPayloadExactlyAtTheLimit pins the boundary from the other side: a payload that fits the inline limit exactly is handed back as the structure itself, not spilled.
func TestBoundedStructuredContentKeepsAPayloadExactlyAtTheLimit(t *testing.T) {
	t.Parallel()

	sc := map[string]any{"blob": "abc"}

	data, err := json.Marshal(sc)
	if err != nil {
		t.Fatal(err)
	}

	got := boundedStructuredContent(sc, len(data), t.TempDir())
	if !reflect.DeepEqual(got, sc) {
		t.Errorf("boundedStructuredContent at exactly the limit = %#v, want the structure untouched", got)
	}

	if _, spilled := boundedStructuredContent(sc, len(data)-1, t.TempDir()).(string); !spilled {
		t.Error("one byte over the limit was not spilled")
	}
}
