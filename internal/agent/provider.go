// Package agent runs an agent step's LLM tool-calling conversation: it
// resolves the agent's model/connection, compiles the step's granted tools,
// drives the request/tool-execute/append loop (see runAgentConversation),
// and enforces required: true tools via the provider's tool_choice. It also
// runs a task's fix: agent (RunFix) on the same conversation machinery.
package agent

import (
	"errors"
	"fmt"
	"os"

	genaiopenai "github.com/achetronic/adk-utils-go/genai/openai"
	"google.golang.org/adk/v2/model"
)

// defaultAgentPersona is the system persona used when an agent doesn't set
// its own `system:`.
const defaultAgentPersona = `You are an automated agent running as one step of a CI pipeline job.`

// agentOperatingNote is appended to the persona to give the model its
// operating context (working directory + tool discipline). Filled with the
// resolved working directory.
const agentOperatingNote = `Your working directory is %s. Use the tools available to you (all scoped to that directory) to complete the task described below. When finished, reply with a final plain-text message and no further tool calls.`

// buildSystemMessage combines an agent's persona with the operating note for
// a given working directory.
func buildSystemMessage(persona, dir string) string {
	if persona == "" {
		persona = defaultAgentPersona
	}

	return persona + "\n\n" + fmt.Sprintf(agentOperatingNote, dir)
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
func newAgentLLM(baseURL, modelName, apiKey string) model.LLM {
	return genaiopenai.New(genaiopenai.Config{
		APIKey:    apiKey,
		BaseURL:   baseURL,
		ModelName: modelName,
	})
}
