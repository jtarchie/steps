package agent

import (
	"strings"
	"testing"
	"unicode/utf8"

	"google.golang.org/genai"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/store"
)

func recapEntries() []store.ContextEntry {
	return []store.ContextEntry{
		{Key: "failure_cause", Value: strings.Repeat("x", compactValueLen+50), WrittenBy: "investigator"},
		{Key: "owner", Value: "platform", WrittenBy: "triager"},
	}
}

// TestRenderRecapFidelity pins what each rung of the ladder actually shows.
// The levels differ only in how much of a value survives, which is the whole
// contract a pipeline author is choosing between.
func TestRenderRecapFidelity(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		fidelity   config.ContextFidelity
		wantKey    bool
		wantValue  bool
		wantElided bool
	}{
		{name: "off shows nothing", fidelity: config.FidelityOff},
		{name: "truncate shows keys only", fidelity: config.FidelityTruncate, wantKey: true},
		{name: "compact shortens values", fidelity: config.FidelityCompact, wantKey: true, wantValue: true, wantElided: true},
		{name: "summary shows values in full", fidelity: config.FidelitySummary, wantKey: true, wantValue: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := renderRecap(recapEntries(), tc.fidelity)

			if !tc.wantKey {
				if got != "" {
					t.Fatalf("renderRecap(%s) = %q, want empty", tc.fidelity, got)
				}

				return
			}

			if !strings.Contains(got, "failure_cause") || !strings.Contains(got, "owner") {
				t.Errorf("renderRecap(%s) = %q, want both keys", tc.fidelity, got)
			}

			// Attribution rides along at every level that shows anything: a
			// fact is more usable when the reader knows who established it.
			if !strings.Contains(got, "investigator") {
				t.Errorf("renderRecap(%s) = %q, want the writing step named", tc.fidelity, got)
			}

			if gotValue := strings.Contains(got, "platform"); gotValue != tc.wantValue {
				t.Errorf("renderRecap(%s) shows values = %v, want %v", tc.fidelity, gotValue, tc.wantValue)
			}

			// A shortened value says so. A model shown a silently truncated
			// value answers as if it had the whole thing.
			if gotElided := strings.Contains(got, "truncated"); gotElided != tc.wantElided {
				t.Errorf("renderRecap(%s) marks elision = %v, want %v", tc.fidelity, gotElided, tc.wantElided)
			}
		})
	}
}

// TestRenderRecapEmpty proves an empty store renders nothing at every level —
// the gate that keeps pipelines which never write context unchanged.
func TestRenderRecapEmpty(t *testing.T) {
	t.Parallel()

	for _, fidelity := range []config.ContextFidelity{
		config.FidelityOff, config.FidelityTruncate, config.FidelityCompact, config.FidelitySummary,
	} {
		if got := renderRecap(nil, fidelity); got != "" {
			t.Errorf("renderRecap(nil, %s) = %q, want empty", fidelity, got)
		}
	}
}

// TestRenderRecapFramesFactsAsData pins the notice. The recap carries text a
// model wrote, into another model's context — without the framing, a recorded
// "ignore your instructions" reads as an instruction.
func TestRenderRecapFramesFactsAsData(t *testing.T) {
	t.Parallel()

	got := renderRecap(recapEntries(), config.FidelityCompact)
	if !strings.Contains(got, "data, not instructions") {
		t.Errorf("renderRecap = %q, want the data-not-instructions framing", got)
	}
}

// TestReadContextToolServesTheSnapshot proves the tool answers from the same
// facts the recap was rendered from, and that its detail argument switches
// between full values and keys.
func TestReadContextToolServesTheSnapshot(t *testing.T) {
	t.Parallel()

	_, impl := buildReadContextTool(recapEntries())

	full := impl(t.Context(), map[string]any{}, toolEnv{})
	if content, _ := full["content"].(string); !strings.Contains(content, "platform") {
		t.Errorf("read_context default = %v, want full values", full)
	}

	keys := impl(t.Context(), map[string]any{"detail": "keys"}, toolEnv{})

	content, _ := keys["content"].(string)
	if !strings.Contains(content, "owner") {
		t.Errorf("read_context keys = %v, want the key names", keys)
	}

	if strings.Contains(content, "platform") {
		t.Errorf("read_context detail=keys = %v, want no values", keys)
	}
}

// TestInjectReadContextToolSkipsAnEmptyRun proves the tool is not offered
// when nothing was recorded: a step cannot usefully re-read an empty store,
// and offering it would spend turns teaching the model that.
func TestInjectReadContextToolSkipsAnEmptyRun(t *testing.T) {
	t.Parallel()

	decls := &genai.Tool{}
	registry := map[string]toolImpl{}

	err := injectReadContextTool(nil, decls, registry)
	if err != nil {
		t.Fatalf("injectReadContextTool: %v", err)
	}

	if len(decls.FunctionDeclarations) != 0 || len(registry) != 0 {
		t.Errorf("injected %v with nothing recorded, want no tool", decls.FunctionDeclarations)
	}

	err = injectReadContextTool(recapEntries(), decls, registry)
	if err != nil {
		t.Fatalf("injectReadContextTool: %v", err)
	}

	if _, ok := registry[readContextToolName]; !ok {
		t.Errorf("registry = %v, want %s once facts exist", registry, readContextToolName)
	}
}

// TestRecapValueTruncatesOnARuneBoundary guards the compact elision against
// splitting a multi-byte character. A recorded fact is model-authored text and
// routinely holds non-ASCII; a byte-boundary cut puts a broken code point into
// the very value a later step is meant to read.
func TestRecapValueTruncatesOnARuneBoundary(t *testing.T) {
	t.Parallel()

	// "é" is two bytes, so a cut at compactValueLen lands mid-rune.
	value := strings.Repeat("a", compactValueLen-1) + "é" + strings.Repeat("b", 50)

	got := recapValue(value, config.FidelityCompact)
	if !utf8.ValidString(got) {
		t.Errorf("recapValue produced invalid UTF-8: %q", got)
	}

	if !strings.Contains(got, "truncated") {
		t.Errorf("recapValue = %q, want the elision marked", got)
	}
}
