// Package agent runs an agent step's LLM tool-calling conversation: it
// resolves the agent's model/connection, compiles the step's granted tools,
// drives the request/tool-execute/append loop (see runAgentConversation),
// and enforces required: true tools via the provider's tool_choice. It also
// runs a task's fix: agent (RunFix) on the same conversation machinery.
package agent

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	genaiopenai "github.com/achetronic/adk-utils-go/genai/openai"
	"google.golang.org/adk/v2/model"

	"github.com/jtarchie/steps/internal/config"
)

// defaultAgentPersona is the system persona used when an agent doesn't set
// its own `system:`.
const defaultAgentPersona = `You are an automated agent running as one step of a CI pipeline job.`

// agentOperatingNote is appended to the persona to give the model its
// operating context (working directory + tool discipline). Filled with the
// resolved working directory.
const agentOperatingNote = `Your working directory is %s. Use the tools available to you (all scoped to that directory) to complete the task described below. When finished, reply with a final plain-text message and no further tool calls.`

// contextBlock is one resolved context_paths: file — the path as declared
// (shown to the model, so it can cite or re-read the file) and its full
// contents.
type contextBlock struct {
	path    string
	content string
}

// buildSystemMessage combines an agent's persona with the operating note
// for a given working directory. context_paths content is no longer injected
// here — it is delivered as synthetic read_file tool results (see
// buildAgentRequest).
func buildSystemMessage(persona, dir string) string {
	if persona == "" {
		persona = defaultAgentPersona
	}

	return persona + "\n\n" + fmt.Sprintf(agentOperatingNote, dir)
}

// loadContextBlocks reads an agent's declared context_paths out of the
// step's working directory at preparation time. Every path is confined to
// the workspace by resolveAgentPath — the same guard the file tools use —
// and capped at maxReadFileBytes, the size read_file already treats as the
// largest sane in-context document. A missing, escaping, or oversized file
// is a hard preparation error: context_paths is operator-authored config,
// so a bad one — missing, or escaping the workspace — should fail the step
// loudly before a token is spent, rather than surface as a surprise
// mid-conversation. A file that is merely too BIG is not that: see below.
func loadContextBlocks(dir string, paths []string) ([]contextBlock, error) {
	if len(paths) == 0 {
		return nil, nil
	}

	blocks := make([]contextBlock, 0, len(paths))

	for _, p := range paths {
		resolved, err := resolveAgentPath(dir, p)
		if err != nil {
			return nil, fmt.Errorf("context path %q: %w", p, err)
		}

		data, err := os.ReadFile(resolved) //nolint:gosec // resolveAgentPath confines to dir
		if err != nil {
			return nil, fmt.Errorf("context path %q: %w", p, err)
		}

		// An oversized file is TRUNCATED, not refused.
		//
		// It used to fail the step, on the reasoning that context_paths: is
		// operator-authored config and a bad one should be loud. But the
		// operator authors a PATH, not a size: `pr/pr.diff` is a perfectly
		// correct path that fails the moment the pull request it names grows
		// past the limit — which is nothing they did, and is likeliest exactly
		// when the review matters most. A matrix cell's path is model-authored
		// now besides.
		//
		// So it degrades the way read_file does for the same reason: hand over
		// what fits, and say plainly that there is more and how to reach it.
		// Losing the tail of a diff costs a reviewer some context; failing the
		// step costs the entire review.
		content := string(data)
		if len(data) > maxReadFileBytes {
			content = string(data[:maxReadFileBytes]) + fmt.Sprintf(
				"\n\n[truncated: %s is %d bytes, and the first %d are shown. "+
					"Use read_file with start_line/end_line to page through the rest.]",
				p, len(data), maxReadFileBytes)

			slog.Warn("agent.context_path_truncated", "path", p,
				"bytes", len(data), "limit", maxReadFileBytes)
		}

		blocks = append(blocks, contextBlock{path: p, content: content})
	}

	return blocks, nil
}

// lookupAPIKey reads the API key from the OS environment variable named by
// envVar. When required, a missing/empty variable (or a missing envVar
// name) is a hard error — sending a blank Authorization header would just
// produce a confusing 401 from the endpoint. When not required (local
// providers with no default key), a missing key resolves to "" and no
// Authorization header is sent.
func lookupAPIKey(envVar string, required bool) (string, error) {
	if envVar == "" {
		if required {
			return "", errors.New("agent source is missing api_key_env")
		}

		return "", nil
	}

	val, ok := os.LookupEnv(envVar)
	if !ok || val == "" {
		if required {
			return "", fmt.Errorf("environment variable %q (api_key_env) is not set", envVar)
		}

		return "", nil
	}

	return val, nil
}

// newAgentLLM constructs the model.LLM used for real runs, given the
// already-resolved base URL/model/key (see config.ResolveAgentInvocation).
// Returning the model.LLM interface (not the concrete *openai.Model) keeps
// runAgentConversation testable against an in-process fake.
//
// Every invocation gets the package's shared HTTP client (see
// agentHTTPClient): the tool-call argument repair transport applies to all
// providers, and an OpenRouter base URL additionally gets the session/cache
// transport (see openrouter.go).
//
// It takes the whole ResolvedInvocation rather than loose strings so the
// mapping from invocation fields to client settings lives in exactly one
// place — the three call sites (RunStep, RunFix, buildSubAgentTool) can't
// individually mix up which field feeds which parameter.
func newAgentLLM(ri config.ResolvedInvocation, apiKey string) model.LLM {
	cfg := genaiopenai.Config{
		APIKey:    apiKey,
		BaseURL:   ri.BaseURL,
		ModelName: ri.ModelName,
		HTTPOptions: genaiopenai.HTTPOptions{
			Client: agentHTTPClient(ri),
		},
	}

	return genaiopenai.New(cfg)
}

// agentHTTPClient returns the *http.Client an invocation's LLM uses. Its
// transport stack is, innermost out: the shared base transport (one
// process-wide connection pool, see agentBaseTransport), the request-retry
// transport that implements attempts: (see requests.go — innermost, because an
// individual request is the operation it retries), the tool-call argument
// repair transport (all providers — see repair.go), and, only for an
// OpenRouter base URL, the session/cache transport (see openrouter.go). Split
// out from newAgentLLM so the field mapping it performs — the session is
// scoped by AgentName, not ModelName — is directly assertable in a test.
func agentHTTPClient(ri config.ResolvedInvocation) *http.Client {
	retrying := &requestRetryTransport{
		base:     agentBaseTransport(),
		agent:    ri.AgentName,
		model:    ri.ModelName,
		attempts: ri.Attempts,
	}

	var transport http.RoundTripper = &repairTransport{base: retrying}

	if isOpenRouterBaseURL(ri.BaseURL) {
		transport = &openRouterTransport{base: transport, agent: ri.AgentName}
	}

	return &http.Client{Transport: transport}
}
