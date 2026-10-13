// Package agent runs an agent step's LLM tool-calling conversation: it
// resolves the agent's model/connection, compiles the step's granted tools,
// drives the request/tool-execute/append loop (see runAgentConversation),
// and enforces required: true tools via the provider's tool_choice. It also
// runs a task's fix: agent (RunFix) on the same conversation machinery.
package agent

import (
	"fmt"
	"time"
)

// defaultAgentPersona is the system persona used when an agent doesn't set
// its own `system:`.
const defaultAgentPersona = `You are an automated agent running as one step of a CI pipeline job.`

// agentOperatingNote is appended to the persona to give the model its
// operating context (working directory + tool discipline). Filled with the
// resolved working directory.
const agentOperatingNote = `Your working directory is %s. Use the tools available to you (all scoped to that directory) to complete the task described below. When finished, reply with a final plain-text message and no further tool calls.`

// agentTimeoutNote discloses this conversation's wall-clock deadline, so the
// model can pace itself instead of being cut off mid-request with no warning
// it was ever running against a clock. Appended only when a deadline
// actually applies (see buildSystemMessage) — an agent step's timeout: is
// otherwise invisible to the model, which used to learn about it only by
// being cut off.
const agentTimeoutNote = `This conversation has a wall-clock deadline: it must finish within %s from now, or it will be cut off mid-request with no chance to respond.`

// buildSystemMessage combines an agent's persona with the operating note
// for a given working directory, plus a timeout disclosure when timeout is
// a real deadline (not noAgentDeadline — timeout: "0"). context_paths
// content is no longer injected here — it is delivered as synthetic
// read_file tool results (see buildAgentRequest).
//
// Folded into the system message rather than appended as an ordinary user
// turn: SystemInstruction is separate from req.Contents and survives
// maybeCompact untouched, so the disclosure is still visible late into a
// long, compacted conversation.
func buildSystemMessage(persona, dir string, timeout time.Duration) string {
	if persona == "" {
		persona = defaultAgentPersona
	}

	msg := persona + "\n\n" + fmt.Sprintf(agentOperatingNote, dir)

	if timeout != noAgentDeadline {
		msg += "\n\n" + fmt.Sprintf(agentTimeoutNote, timeout.Round(time.Second))
	}

	return msg
}
