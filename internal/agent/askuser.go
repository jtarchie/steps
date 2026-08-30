package agent

// ask_user: the built-in that lets an agent say it does not know something
// and get an answer.
//
// Every other tool answers from the machine. This one answers from a person
// (or from an agent standing in for one), which makes it the only tool whose
// implementation has to survive waiting: the row is written BEFORE anyone is
// asked, so nothing about the audit trail depends on which channel answered,
// or on this process still being alive when one does.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"google.golang.org/genai"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
)

const (
	// defaultAskUserWait bounds a wait nobody bounded. Much shorter than
	// approval:'s day, because the two park for different reasons: an
	// approval gates a deploy somebody will come back to, while a question
	// gates a step that is holding a workspace, a container and possibly a
	// billed worker open while it waits.
	defaultAskUserWait = 30 * time.Minute
	// askUserPollInterval is how often a parked question re-reads its row. A
	// person is answering, so seconds are free.
	askUserPollInterval = 2 * time.Second
	// askUserQuestionArg / askUserOptionsArg are the tool's two arguments.
	askUserQuestionArg = "question"
	askUserOptionsArg  = "options"
	// cliMCPToolTimeoutEnv is what the claude CLI reads for its per-tool-call
	// deadline on an MCP tool. See cliToolTimeoutEnv for why a parked question
	// has to widen it.
	cliMCPToolTimeoutEnv = "MCP_TOOL_TIMEOUT"
	// cliToolTimeoutMargin keeps the child's deadline strictly longer than the
	// wait it is covering, so the timeout that fires is ask_user's own.
	cliToolTimeoutMargin = time.Minute
)

// askUserWait is how long a grant says a person may be waited on, which is the
// grant's timeout: or the package default. Read from the specs rather than
// from the built grant because the CLI path needs it while assembling the
// subprocess environment, by which point only the declaration is in hand.
func askUserWait(specs []config.ToolSpec) time.Duration {
	for _, spec := range specs {
		if spec.Builtin != config.AskUserBuiltinName || spec.Timeout == "" {
			continue
		}

		wait, err := config.ParseTimeout(spec.Timeout)
		if err == nil && wait > 0 {
			return wait
		}
	}

	return defaultAskUserWait
}

// askUserDescription is prompt surface, and is written to say the two things
// a model gets wrong about a tool like this: WHEN to reach for it (only for a
// fact it cannot obtain any other way — not to check its work, and not to ask
// permission) and that the answer is not free.
const askUserDescription = "Ask the person running this pipeline a question, and wait for their answer." +
	" Use it only for a fact you cannot get any other way — something the pipeline never stated and no file, command or" +
	" page can tell you — such as which of several readings of an ambiguous instruction was meant. Do NOT use it to" +
	" confirm work you can verify yourself, to ask permission for something you were already told to do, or to report" +
	" progress. Offer options when the answer is one of a few known values; the person may still answer in their own" +
	" words. Somebody is waiting on this call, so ask once, ask specifically, and include the context they need to" +
	" answer without reading your transcript. If nobody answers in time you are told so, and you should proceed on" +
	" your best reading and say in your final answer what you assumed."

// askGrant is one ask_user grant, resolved: the dials from the tools: entry
// plus the responder agent (if any) built ready to run.
type askGrant struct {
	answeredBy      string
	responder       toolImpl
	defaultAnswer   string
	optionsRequired bool
	wait            time.Duration
}

// askEnv is what the ask_user impl needs from the RUN rather than from the
// grant: where to record the question, and who is asking. It rides on toolEnv
// because that is what already reaches every toolImpl, and it is zero only for
// a caller with genuinely no run to record against (a test, a direct call) —
// which the tool reports as data rather than pretending to park.
//
// Every conversation inside a run has one: a plan step gets it from RunStep, a
// sub-agent inherits its parent's (renamed, see forAgent), and a task fix: or
// a hook agent gets it from askContext. Being outside the merkle chain, as a
// hook is, says nothing about whether there is somebody to ask.
type askEnv struct {
	st        *store.Store
	jobName   string
	agentName string
	// prompt is the terminal channel: a function that puts the question to
	// somebody at a TTY and returns their answer. Nil when stdin is not a
	// terminal, which is every CI run and every test. It is a field rather
	// than a package-level hook so a test can supply one without a TTY and
	// without mutating shared state.
	prompt askPrompter
	// state carries the one thing a tool call cannot report as data: that
	// nobody answered a question with no default, so the step must abort.
	state *askState
}

// askContext is the ask environment for a conversation that is NOT a plan
// step — a task fix: agent, a hook — but is still inside a recorded run.
//
// It exists because the distinction that matters is having a run to park a
// question against, and those conversations have one: only the store handle
// was missing, three frames up its own call chain. A nil store still yields a
// usable zero value, so a caller with genuinely nothing to record against
// (a test, a direct call) degrades to the tool saying so as data.
func askContext(st *store.Store, jobName, agentName string) askEnv {
	return askEnv{
		st: st, jobName: jobName, agentName: agentName,
		prompt: terminalPrompter(), state: &askState{},
	}
}

// forAgent re-labels this ask context for a nested conversation. A sub-agent
// asks on behalf of the same run and records against the same store; only the
// name on the row changes, so a person reading `steps questions` sees the
// agent that actually wants to know.
func (e askEnv) forAgent(name string) askEnv {
	if name != "" {
		e.agentName = name
	}

	return e
}

// askPrompter puts a question to a person at a terminal. It returns the
// answer and whether one was given; a false means "not from here" (no
// terminal, the wait ended first, or the reader failed), never an error the
// model needs to see.
type askPrompter func(ctx context.Context, question store.Question) (string, bool)

// askState is the abort latch. A tool result is data by contract, so a tool
// can never fail a step — but an expiry with no declared default is exactly
// the case where continuing would mean the model proceeding on a fact nobody
// supplied. The latch is set here and read by the conversation loop, which
// ends the attempt with it (see runConversationLoop).
type askState struct {
	mu  sync.Mutex
	err error
}

func (s *askState) abort(err error) {
	if s == nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// First abort wins: with concurrent tool calls the earliest expiry is the
	// one that describes why the step is ending.
	if s.err == nil {
		s.err = err
	}
}

func (s *askState) aborted() error {
	if s == nil {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.err
}

// buildAskUserTool resolves one ask_user grant into the declaration the model
// sees and the impl that runs the ladder.
//
// The responder is built EAGERLY, like a sub-agent tool: an answered_by:
// naming an agent whose credential is missing must fail the step's
// preparation, not the first question — by which point somebody is already
// waiting.
func buildAskUserTool(ctx context.Context, cfg *config.Config, spec config.ToolSpec) (*genai.FunctionDeclaration, toolImpl, io.Closer, error) {
	grant := askGrant{
		answeredBy:      spec.AnsweredBy,
		defaultAnswer:   spec.Default,
		optionsRequired: spec.OptionsRequired,
		wait:            defaultAskUserWait,
	}

	if spec.Timeout != "" {
		wait, err := config.ParseTimeout(spec.Timeout)
		if err != nil || wait <= 0 {
			return nil, nil, nil, fmt.Errorf("agent tool %q: timeout must be a positive duration (it is how long a person is waited on)", config.AskUserBuiltinName)
		}

		grant.wait = wait
	}

	var closer io.Closer

	if spec.AnsweredBy != "" {
		if cfg == nil {
			return nil, nil, nil, errors.New("ask_user: answered_by requires config to resolve the responding agent")
		}

		// Built through the sub-agent machinery because a responder IS a
		// nested conversation — same resolution, same failover seam, same
		// budget accounting. What differs is not how it runs but what it
		// MEANS: a sub-agent is delegation the asking model chose, a
		// responder is an escalation a person can intercept, and the recorded
		// row is where that difference is written down.
		_, impl, responderCloser, err := buildSubAgentTool(ctx, cfg, config.ToolSpec{Agent: spec.AnsweredBy})
		if err != nil {
			return nil, nil, nil, fmt.Errorf("ask_user: answered_by: %w", err)
		}

		grant.responder, closer = impl, responderCloser
	}

	decl := &genai.FunctionDeclaration{
		Name:        config.AskUserBuiltinName,
		Description: toolDescription(config.AskUserBuiltinName, askUserDescription),
		Parameters: objectSchema(map[string]*genai.Schema{
			askUserQuestionArg: {
				Type:        genai.TypeString,
				Description: "The question, written so somebody who has not read your transcript can answer it. State what you are deciding and why the answer matters.",
			},
			askUserOptionsArg: {
				Type:        genai.TypeArray,
				Items:       &genai.Schema{Type: genai.TypeString},
				Description: "The answers you expect, if the question has a few known ones. The person may still answer in their own words unless the pipeline said otherwise.",
			},
		}, askUserQuestionArg),
	}

	return decl, grant.ask, closer, nil
}

// ask runs the ladder for one question: the run's memo, then a pre-seeded
// answer, then the responder agent, then a person — inline if somebody is at a
// terminal, parked if not.
//
// A question goes to the first channel that can serve it, and the row is
// written before any of them are tried, so a step killed mid-wait still leaves
// behind a record of what it wanted to know.
func (g askGrant) ask(ctx context.Context, args map[string]any, env toolEnv) map[string]any {
	question := strings.TrimSpace(stringArg(args, askUserQuestionArg))
	if question == "" {
		return errorResult("ask_user: question is required — say what you are deciding and why the answer matters")
	}

	options := stringsArg(args, askUserOptionsArg)

	// A fence that admits nothing is not a fence, it is a deadlock: with no
	// options offered, slices.Contains is false for every possible answer, so
	// `steps answer`, the web form and the terminal are ALL refused and the
	// question runs its whole wait out. Told to the model as data, which is the
	// one channel that can still fix it — by asking again, with options.
	if g.optionsRequired && len(options) == 0 {
		return errorResult("ask_user: this grant requires the answer to be one of your offered options, so ask again with a non-empty options list")
	}

	if env.ask.st == nil {
		return errorResult("ask_user: this conversation has no run to record a question against, so there is nobody to ask")
	}

	runID := events.RunID(ctx)
	if runID == "" {
		return errorResult("ask_user: this step is not running inside a recorded run, so there is nobody to ask")
	}

	row, existing, err := env.ask.st.AskQuestion(ctx, store.Question{
		RunID: runID, JobName: env.ask.jobName, AgentName: env.ask.agentName,
		Question: question, Options: options,
		OptionsRequired: g.optionsRequired, Default: g.defaultAnswer,
	})
	if err != nil {
		return errorResult("ask_user: " + err.Error())
	}

	// The memo, and the reason it is the row rather than a map: an answer
	// already given in this run — by another across: cell, by a previous
	// attempt of this same conversation — is returned without asking anybody
	// a second time.
	//
	// Only an ANSWER is memoized, though. A row this run already gave up on
	// (expired with no default, or abandoned when a step ended) carries no
	// answer at all, and handing it back as one would be this design's own
	// audit lie with an empty string in place of the fact — so it resolves the
	// second asker exactly the way it resolved the first.
	if row.Status != "pending" {
		return g.memoResult(env, row)
	}

	if !existing {
		answered := g.answerFromMachines(ctx, env, row)
		if answered != nil {
			return answered
		}
	}

	return g.waitForAnswer(ctx, env, row)
}

// memoResult reports a question this run already resolved. An answered one is
// simply returned; an unanswered one repeats whatever ending the first asker
// got, including its abort — never an empty answer dressed up as a default.
func (g askGrant) memoResult(env toolEnv, row store.Question) map[string]any {
	if row.Status == "answered" || row.Answer != "" {
		return resolvedResult(row, "memo")
	}

	err := fmt.Errorf("question %d was %s with no answer earlier in this run: %s", row.ID, row.Status, row.Question)
	env.ask.state.abort(err)

	return errorResult("ask_user: " + err.Error())
}

// answerFromMachines tries the two channels that need no person: a pre-seeded
// answer, then the responder agent. It returns nil when neither served the
// question, which is what sends it on to a human.
//
// Only the asker walks these — a caller that found somebody else's pending row
// waits on it instead, so two racing cells do not both escalate.
func (g askGrant) answerFromMachines(ctx context.Context, env toolEnv, row store.Question) map[string]any {
	if seed, ok := matchAnswerSeed(ctx, row.Question); ok {
		return g.record(ctx, env, row, seed, "seed", "seed")
	}

	if g.responder == nil {
		return nil
	}

	answer, ok := g.askResponder(ctx, env, row)
	if !ok {
		return nil
	}

	return g.record(ctx, env, row, answer, "agent:"+g.answeredBy, "agent:"+g.answeredBy)
}

// record writes an answer to the row and renders it for the model. A write
// that loses a race (somebody answered in between) is not an error: the row is
// re-read and whatever is actually recorded is what the model is told, since
// the row — not this process — is the answer of record.
func (g askGrant) record(ctx context.Context, env toolEnv, row store.Question, answer, by, source string) map[string]any {
	err := env.ask.st.AnswerQuestion(ctx, row.ID, answer, by)
	if err == nil {
		row.Answer, row.AnsweredBy, row.Status = answer, by, "answered"

		return resolvedResult(row, source)
	}

	if !errors.Is(err, store.ErrQuestionNotPending) {
		return errorResult(fmt.Sprintf("ask_user: %s", err))
	}

	current, readErr := env.ask.st.QuestionStatus(ctx, row.ID)
	if readErr != nil {
		return errorResult(fmt.Sprintf("ask_user: %s", readErr))
	}

	return resolvedResult(current, "raced")
}

// askResponder escalates to the answered_by: agent. Its final text is the
// answer.
//
// A responder that fails, or that answers with nothing, does not fail the
// question — it falls through to a person, which is the whole point of an
// escalation being a ladder rather than a substitution.
func (g askGrant) askResponder(ctx context.Context, env toolEnv, row store.Question) (string, bool) {
	slog.Info("agent.question_escalated", "question", row.ID, "job", row.JobName, "agent", row.AgentName, "responder", g.answeredBy)

	result := g.responder(ctx, map[string]any{subAgentRequestParam: responderRequest(row)}, env)

	answer, ok := result["result"].(string)
	if !ok || strings.TrimSpace(answer) == "" {
		slog.Warn("agent.question_unescalated", "question", row.ID, "responder", g.answeredBy, "error", result["error"])

		return "", false
	}

	return strings.TrimSpace(answer), true
}

// responderRequest is what the responding agent is asked. It says who is
// asking and what the answer will be used for, because a responder given only
// the bare question answers it as a chat message — with reasoning, caveats and
// a preamble — and the whole string becomes the answer of record.
func responderRequest(row store.Question) string {
	request := fmt.Sprintf("The %q agent is running a step of the %q job and needs an answer before it can continue.\n\nIts question: %s",
		row.AgentName, row.JobName, row.Question)

	if len(row.Options) > 0 {
		request += "\n\nThe answers it expects: " + strings.Join(row.Options, ", ")
	}

	return request + "\n\nAnswer with the answer itself and nothing else — no preamble, no reasoning, no closing remark. Your entire reply is recorded verbatim as the answer."
}

// waitForAnswer parks the question until somebody answers it, the wait
// expires, or the step ends.
func (g askGrant) waitForAnswer(ctx context.Context, env toolEnv, row store.Question) map[string]any {
	deadline := time.Now().Add(g.wait)

	announceQuestion(row, g.wait)

	// The terminal channel runs alongside the poll rather than instead of it:
	// a person at this terminal and a person running `steps answer` in another
	// one are both answering the same row, and whichever lands first wins.
	//
	// Cancelled on every exit from this function, which is what lets an
	// abandoned prompt cost nothing: a question answered from another terminal
	// releases the prompt that was waiting on this one, instead of stranding it
	// in a read that can never be cancelled.
	promptCtx, endPrompt := context.WithCancel(ctx)
	defer endPrompt()

	terminal := g.promptTerminal(promptCtx, env, row)

	for {
		current, err := env.ask.st.QuestionStatus(ctx, row.ID)
		if err != nil {
			return errorResult("ask_user: " + err.Error())
		}

		if current.Status != "pending" {
			fmt.Printf("question %d: answered by %s\n", current.ID, current.AnsweredBy)

			return resolvedResult(current, answerSource(current))
		}

		if time.Now().After(deadline) {
			return g.expire(ctx, env, row)
		}

		select {
		case <-ctx.Done():
			// The step is ending — its own timeout, or a Ctrl-C. Mark the row
			// so it stops showing up as something somebody could still
			// answer; an unanswerable question sitting pending forever is the
			// same class of lie as a defaulted answer presented as a person's.
			g.close(ctx, env, row, "aborted", "", "step")

			return errorResult(fmt.Sprintf("ask_user: question %d was abandoned when the step ended", row.ID))
		case answer := <-terminal:
			// A refused answer (one the options fence rejected) leaves the
			// question OPEN rather than ending the wait with an error: the
			// person is still there, `steps answer` can still land, and
			// returning here would strand the row pending with nobody left to
			// resolve it. They are prompted again, since a fence they can
			// satisfy on the second try is the whole reason it is a fence and
			// not a rejection.
			if answer != "" {
				recorded, ok := g.recordTerminal(ctx, env, row, answer)
				if ok {
					return recorded
				}
			}

			terminal = g.promptTerminal(promptCtx, env, row)
		case <-time.After(askUserPollInterval):
		}
	}
}

// recordTerminal writes what somebody typed, reporting whether it stuck. A
// refusal is printed where they can see it — they are at the terminal, which
// is the only channel that can be told anything.
func (g askGrant) recordTerminal(ctx context.Context, env toolEnv, row store.Question, answer string) (map[string]any, bool) {
	err := env.ask.st.AnswerQuestion(ctx, row.ID, answer, terminalAnswerer())
	if err == nil {
		row.Answer, row.AnsweredBy, row.Status = answer, terminalAnswerer(), "answered"

		return resolvedResult(row, "terminal"), true
	}

	if errors.Is(err, store.ErrQuestionNotPending) {
		// Somebody (or something) else got there first; the next poll reads
		// what they recorded.
		return nil, false
	}

	fmt.Printf("question %d: %s\n", row.ID, err)

	return nil, false
}

// promptTerminal starts the terminal channel, if there is one, and returns the
// channel its answer arrives on. A nil prompter (no TTY, which is every CI run)
// yields a channel nothing ever sends on, which the select below simply never
// takes.
//
// Buffered by one and never closed on purpose: when another channel answers
// first, this goroutine is still blocked reading a line nobody will now use,
// and the buffer is what lets it finish and exit rather than block forever on
// a send.
func (g askGrant) promptTerminal(ctx context.Context, env toolEnv, row store.Question) <-chan string {
	answers := make(chan string, 1)

	if env.ask.prompt == nil {
		return answers
	}

	go func() {
		answer, ok := env.ask.prompt(ctx, row)
		if !ok {
			answer = ""
		}

		answers <- answer
	}()

	return answers
}

// expire resolves a question nobody answered in time.
//
// With a default, the model is told — explicitly, in the result — that nobody
// answered and that a default stood in. An indistinguishable default is the
// runtime telling a model that a person confirmed something no person saw.
//
// Without one, the step ABORTS, matching approval: exactly. Aborted, not
// failed: nobody decided anything.
func (g askGrant) expire(ctx context.Context, env toolEnv, row store.Question) map[string]any {
	// The poll sleeps in whole intervals, so "the deadline passed" and "nobody
	// answered" are not the same statement: a person who typed at T-0.1s lands
	// while this loop is asleep, and expiring on the clock alone would discard
	// an answer the row already holds and abort a step somebody just unparked.
	// The write is what decides, not the reading of the clock — which is the
	// same posture record() takes on the other side of the race.
	current, err := env.ask.st.QuestionStatus(ctx, row.ID)
	if err == nil && current.Status != "pending" {
		return resolvedResult(current, answerSource(current))
	}

	slog.Warn("agent.question_expired", "question", row.ID, "job", row.JobName,
		"agent", row.AgentName, "timeout", g.wait.String(), "default", g.defaultAnswer)

	if g.defaultAnswer == "" {
		g.close(ctx, env, row, "expired", "", "")

		err := fmt.Errorf("question %d expired unanswered after %s and declared no default: %s",
			row.ID, g.wait, row.Question)
		env.ask.state.abort(err)

		return errorResult("ask_user: " + err.Error())
	}

	g.close(ctx, env, row, "expired", g.defaultAnswer, "default")

	return map[string]any{
		"exit_code": 0,
		"answered":  false,
		"answer":    g.defaultAnswer,
		"source":    "default",
		"note": fmt.Sprintf("nobody answered within %s; the declared default was used. Say in your final answer that you proceeded on a default.",
			g.wait),
	}
}

// close resolves a row on a path where nobody is left to report a failure to.
//
// On a context stripped of cancellation, because the likeliest reason this
// runs is that the step's context was just cancelled — and a write that
// aborted before reaching sqlite would leave behind exactly the pending row it
// exists to clear.
func (g askGrant) close(ctx context.Context, env toolEnv, row store.Question, status, answer, by string) {
	err := env.ask.st.CloseQuestion(context.WithoutCancel(ctx), row.ID, status, answer, by)
	if err != nil && !errors.Is(err, store.ErrQuestionNotPending) {
		slog.Warn("agent.question_unresolved", "question", row.ID, "status", status, "error", err)
	}
}

// announceQuestion is the last line anybody sees before the step stops making
// progress, so it carries the exact command to answer it — the same reasoning
// as approval:'s.
func announceQuestion(row store.Question, wait time.Duration) {
	fmt.Printf("question %d: %s\n", row.ID, row.Question)

	if len(row.Options) > 0 {
		fmt.Printf("question %d: options: %s\n", row.ID, strings.Join(row.Options, " | "))
	}

	fmt.Printf("question %d: waiting up to %s — steps answer <pipeline> %d <answer>\n", row.ID, wait, row.ID)

	slog.Warn("agent.question_pending", "question", row.ID, "job", row.JobName,
		"agent", row.AgentName, "question_text", row.Question, "timeout", wait.String())
}

// resolvedResult renders a resolved row for the model. answered: is the field
// that carries the honesty — false is how the model learns a default or an
// abandoned question stood in for an answer nobody gave.
func resolvedResult(row store.Question, source string) map[string]any {
	result := map[string]any{
		"exit_code": 0,
		"answered":  row.Status == "answered",
		"answer":    row.Answer,
		"source":    source,
	}

	if row.Status != "answered" {
		result["note"] = "nobody answered this question; " + row.Answer + " was used because the pipeline declared it as the default"
	}

	return result
}

// answerSource labels an answer that arrived while this call was parked, which
// is all this process knows about it: who the row says answered.
func answerSource(row store.Question) string {
	if row.AnsweredBy == "" {
		return "waited"
	}

	return row.AnsweredBy
}

func errorResult(message string) map[string]any {
	return map[string]any{"error": message}
}

// stringsArg reads a list-of-strings tool argument. The model's arguments
// arrive as []any through JSON; a Go caller (tests, the bridge) may pass
// []string directly. Anything else — including a bare string where a list was
// asked for — is read as no options rather than guessed at.
func stringsArg(args map[string]any, key string) []string {
	switch v := args[key].(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))

		for _, entry := range v {
			if s, ok := entry.(string); ok && s != "" {
				out = append(out, s)
			}
		}

		return out
	default:
		return nil
	}
}
