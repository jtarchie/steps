package config

// What a model's context window is, and where in it an agent should compact.

import (
	"strings"
)

// defaultCompactAfterTokens is the conversation-size budget (in estimated
// tokens — see estimateContentTokens in internal/agent/compaction.go) an
// agent that sets no compact_after_tokens: gets: 80% of a 128K context
// window, the common size for current models.
//
// The 20% headroom is load-bearing, not padding. estimateContentTokens
// measures req.Contents alone — it never counts the system prompt, the tool
// schemas re-sent on every request, or the output the model still has to
// fit. A budget set at the full window would only ever fire after the
// request had already overflowed.
//
// It is only the fallback for a model whose window this package does not
// recognize (see contextWindowFor) and whose agent declared no
// context_window:. A small local model (32K and under) overflows well before
// this and must state its context_window: (or set compact_after_tokens:
// lower); compact_after_tokens: 0 disables compaction entirely.
const defaultCompactAfterTokens = defaultContextWindow * compactBudgetPercent / 100 // 102,400

// defaultContextWindow is the window assumed for an unrecognized model: the
// common size for current models, and conservative for anything larger.
const defaultContextWindow = 128_000

// compactBudgetPercent is how much of a model's context window compaction is
// allowed to fill before it fires.
const compactBudgetPercent = 80

// contextWindows maps a fragment of a model name to that model's context
// window, most specific first. Matching is on a substring of the NORMALIZED
// model name (see normalizeModelName) with any provider prefix already
// stripped, so it works the same whether the model was written as
// `claude-sonnet-4-5`, `claude-sonnet-4.5`, or
// `openrouter/anthropic/claude-sonnet-4.5` — fragments are therefore spelled
// in the dashed form, never the dotted one.
//
// The point of the table is that a default budget must not be a guess about
// somebody else's model. Compaction defaults ON at 80% of the window, so an
// assumed 128K applied to a 1M-context model compacts at roughly a tenth of
// capacity — silently, forever, paying for a summarization call that buys
// nothing and losing conversation fidelity for no reason.
//
// An entry is only worth adding for a model whose window is confidently known;
// anything unrecognized keeps the conservative 128K default, which is the safe
// direction to be wrong in. That rule decides the shape of the Anthropic
// block below: the 1M models are enumerated individually and the `claude`
// family entry stays at 200K, so a claude-* released after this table was
// written is under-budgeted rather than over-budgeted.
//
// The numbers are the models' own windows, taken from models.dev (the catalog
// opencode itself resolves against; `curl -s https://models.dev/api.json` and
// read .<provider>.models.<id>.limit.context). A HOST that serves a model with
// a smaller window than the model has natively is not something a table keyed
// on model name can express — that is what an explicit context_window: on the
// agent is for, and so is a local model nobody's catalog has heard of.
//
//nolint:gochecknoglobals // static, read-only lookup table
var contextWindows = []struct {
	fragment string
	window   int
}{
	// Anthropic ships 1M-context variants of some models under a suffixed id;
	// checked before the family names below, which would otherwise match first.
	{"[1m]", 1_000_000},

	// Anthropic. Everything from sonnet-4-5 forward is natively 1M; the family
	// entry catches the ones that are not (opus-4-5, haiku-4-5, opus-4-1,
	// 3-5-haiku) and anything newer than this table.
	{"claude-fable-5", 1_000_000},
	{"claude-opus-5", 1_000_000},
	{"claude-opus-4-8", 1_000_000},
	{"claude-opus-4-7", 1_000_000},
	{"claude-opus-4-6", 1_000_000},
	{"claude-sonnet-5", 1_000_000},
	{"claude-sonnet-4-6", 1_000_000},
	{"claude-sonnet-4-5", 1_000_000},
	{"claude", 200_000},

	// OpenAI. The gpt-5 family split: 5.4-and-up went to ~1M, but 5.4's own
	// mini/nano stayed at 400K and codex-spark at 128K, so those precede their
	// family entries.
	{"gpt-5-4-mini", 400_000},
	{"gpt-5-4-nano", 400_000},
	{"gpt-5-3-codex-spark", 128_000},
	{"gpt-5-6", 1_050_000},
	{"gpt-5-5", 1_050_000},
	{"gpt-5-4", 1_050_000},
	{"gpt-5", 400_000},
	{"gpt-4-1", 1_000_000},
	{"gpt-4o", 128_000},
	{"o3", 200_000},
	{"o4-mini", 200_000},

	{"gemini", 1_000_000},

	// xAI. grok-code/grok-build are 256K; the family entry is set to that
	// rather than to grok-4-5's 500K so a new grok lands low, not high.
	{"grok-4-5", 500_000},
	{"grok", 256_000},

	// Open-weight and other hosted families, each family entry pinned to the
	// SMALLEST window that family is served with (glm-5.1 is 202,752 on one
	// host and 204,800 on another; minimax-m3 is 512K on some, 1M on others).
	{"kimi-k3", 1_000_000},
	{"kimi", 262_144},
	{"glm-5-2", 1_000_000},
	{"glm", 200_000},
	{"minimax-m3", 512_000},
	{"minimax", 200_000},
	{"deepseek-v4", 1_000_000},
	{"qwen", 262_144},
	{"mimo", 200_000},
	{"hy3", 190_000},

	// Local-server staples. These match the 128K default; they earn their
	// place by making it a DERIVED window rather than an assumed one, which is
	// the difference the compaction log line reports.
	{"gpt-oss", 131_072},
	{"llama-3-1", 128_000},
	{"llama-3-3", 128_000},
}

// normalizeModelName folds the spellings of one model onto each other before
// the table is consulted: lowercase, and "." to "-" because the same model
// reaches this code as `claude-sonnet-4-5` from Anthropic and opencode but
// `claude-sonnet-4.5` from OpenRouter (likewise glm-5.2/glm-5-2,
// qwen3.7-max/qwen3-7-max). Without it every dotted id silently misses and
// falls back to the 128K assumption.
func normalizeModelName(modelName string) string {
	return strings.ReplaceAll(strings.ToLower(modelName), ".", "-")
}

// resolveCompactionBudget decides an invocation's conversation-size budget and
// reports the context window it was derived from (0 when the model is not one
// this package recognizes and the agent declared no context_window:, so a
// caller can tell "1M, derived from the model" from "128K, assumed because we
// have never heard of this model").
//
// Precedence: an explicit compact_after_tokens: always wins; otherwise the
// budget is compactBudgetPercent of the window — an explicit context_window:
// if the agent set one, else a recognized window from the table; otherwise the
// conservative default. Deriving it is the whole point — compaction defaults
// ON, so an assumed 128K applied to a 1M model compacted at a tenth of
// capacity, silently and forever, paying for a summarization call that bought
// nothing.
//
// The two overrides are not redundant. context_window: says what the model IS
// and lets the percentage keep applying; compact_after_tokens: overrides the
// result outright. An operator who knows their host serves 200K of a 1M model
// wants the former and should not have to compute 160,000 by hand.
func resolveCompactionBudget(modelName string, explicitWindow int, explicit *int) (budget, contextWindow int) {
	window, known := contextWindowFor(modelName)

	if explicitWindow > 0 {
		window, known = explicitWindow, true
	}

	budget = defaultCompactAfterTokens
	if known {
		budget = window * compactBudgetPercent / 100
		contextWindow = window
	}

	if explicit != nil {
		budget = *explicit
	}

	return budget, contextWindow
}

// contextWindowFor returns the context window known for a model name, and
// whether it was recognized at all. The caller distinguishes the two: a
// recognized window sets the compaction budget, an unrecognized one leaves
// today's conservative default in place.
func contextWindowFor(modelName string) (int, bool) {
	name := normalizeModelName(modelName)

	for _, entry := range contextWindows {
		if strings.Contains(name, entry.fragment) {
			return entry.window, true
		}
	}

	return defaultContextWindow, false
}
