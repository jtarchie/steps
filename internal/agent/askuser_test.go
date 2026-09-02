package agent

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
)

// askFixture is one ask_user call's worth of world: a store with a live run to
// record against, and the env the tool reads its run identity from.
type askFixture struct {
	store *store.Store
	env   toolEnv
	ctx   context.Context //nolint:containedctx // the run identity is a context value; every call in a case shares it
}

func newAskFixture(t *testing.T) *askFixture {
	t.Helper()

	st, err := store.OpenStore(filepath.Join(t.TempDir(), "state.db"), "test")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })

	ctx := events.WithRunID(t.Context(), "run-1")

	err = st.StartRun(ctx, "run-1", "release-note", "/tmp/ws", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	return &askFixture{
		store: st,
		ctx:   ctx,
		env: toolEnv{ask: askEnv{
			st: st, jobName: "release-note", agentName: "writer", state: &askState{},
		}},
	}
}

// ask runs one ask_user call against the fixture.
func (f *askFixture) ask(grant askGrant, question string, options ...string) map[string]any {
	args := map[string]any{askUserQuestionArg: question}
	if len(options) > 0 {
		args[askUserOptionsArg] = options
	}

	return grant.ask(f.ctx, args, f.env)
}

// impatient is a grant that gives up immediately, so a test about what happens
// when nobody answers does not spend the wait waiting.
func impatient(defaultAnswer string) askGrant {
	return askGrant{defaultAnswer: defaultAnswer, wait: time.Millisecond}
}

// TestAskUserSeededAnswerSkipsEveryoneElse: a seeded answer is the first rung,
// and it resolves the row rather than only the call — so the next asker (an
// across: cell, a retried attempt) finds it recorded.
func TestAskUserSeededAnswerSkipsEveryoneElse(t *testing.T) {
	t.Parallel()

	fixture := newAskFixture(t)
	fixture.ctx = WithAnswerSeeds(fixture.ctx, []AnswerSeed{{Match: "minor bump", Answer: "minor"}})

	result := fixture.ask(impatient(""), "Is this a major or a minor bump?")

	if result["answer"] != "minor" || result["source"] != "seed" || result["answered"] != true {
		t.Errorf("seeded ask = %v, want answered/minor/seed", result)
	}

	pending, err := fixture.store.PendingQuestions(fixture.ctx)
	if err != nil {
		t.Fatalf("PendingQuestions: %v", err)
	}

	if len(pending) != 0 {
		t.Errorf("a seeded question stayed pending: %+v", pending)
	}
}

// TestAskUserMemoAnswersTheSecondAsker is the across:/in_parallel:/attempts:
// case: the same question asked twice in one run reaches the recorded answer
// without anybody being asked again. The second call uses a grant that would
// give up instantly, so a memo miss shows up as a default rather than as a
// slow pass.
func TestAskUserMemoAnswersTheSecondAsker(t *testing.T) {
	t.Parallel()

	fixture := newAskFixture(t)
	fixture.ctx = WithAnswerSeeds(fixture.ctx, []AnswerSeed{{Match: "bump", Answer: "minor"}})

	fixture.ask(impatient(""), "Is this a major or a minor bump?")

	// A fresh grant with no seeds at all, standing in for another cell.
	fixture.ctx = WithAnswerSeeds(t.Context(), nil)
	fixture.ctx = events.WithRunID(fixture.ctx, "run-1")

	second := fixture.ask(impatient("patch"), "Is this a major or a minor bump?")

	if second["answer"] != "minor" || second["source"] != "memo" {
		t.Errorf("the second asker got %v, want the recorded minor from the memo", second)
	}
}

// TestAskUserExpiryUsesTheDeclaredDefaultAndSaysSo: the model is TOLD. An
// indistinguishable default is the runtime telling a model a person confirmed
// something no person saw.
func TestAskUserExpiryUsesTheDeclaredDefaultAndSaysSo(t *testing.T) {
	t.Parallel()

	fixture := newAskFixture(t)

	result := fixture.ask(impatient("patch"), "Which bump?")

	if result["answer"] != "patch" || result["source"] != "default" {
		t.Errorf("expired ask = %v, want the declared default", result)
	}

	if result["answered"] != false {
		t.Error("an expired question reported answered: true; nobody answered it")
	}

	if note, _ := result["note"].(string); !strings.Contains(note, "nobody answered") {
		t.Errorf("expired ask note = %q, want it to say nobody answered", note)
	}

	if fixture.env.ask.state.aborted() != nil {
		t.Error("a question with a declared default aborted the step")
	}
}

// TestAskUserExpiryWithoutADefaultAbortsTheStep: aborted, not failed — nobody
// decided anything. The latch is the only way a tool can say that, since a
// tool result is data by contract.
func TestAskUserExpiryWithoutADefaultAbortsTheStep(t *testing.T) {
	t.Parallel()

	fixture := newAskFixture(t)

	result := fixture.ask(impatient(""), "Which bump?")

	if _, isError := result["error"]; !isError {
		t.Errorf("an unanswerable expiry returned %v, want an error result", result)
	}

	abortErr := fixture.env.ask.state.aborted()
	if abortErr == nil {
		t.Fatal("an expiry with no default did not set the abort latch, so the step would carry on without the fact")
	}

	if !strings.Contains(abortErr.Error(), "expired unanswered") {
		t.Errorf("abort error = %q, want it to name the unanswered question", abortErr)
	}
}

// TestAskUserRecordsWhatWasOfferedBeforeAnybodyAnswers: the row is written
// first, so a step killed mid-wait still leaves behind what it wanted to know.
func TestAskUserRecordsWhatWasOfferedBeforeAnybodyAnswers(t *testing.T) {
	t.Parallel()

	fixture := newAskFixture(t)

	fixture.ask(impatient("patch"), "Which bump?", "major", "minor", "patch")

	questions, err := fixture.store.PendingQuestions(fixture.ctx)
	if err != nil {
		t.Fatalf("PendingQuestions: %v", err)
	}

	if len(questions) != 0 {
		t.Fatalf("the expired question is still pending: %+v", questions)
	}

	recorded, err := fixture.store.QuestionStatus(fixture.ctx, 1)
	if err != nil {
		t.Fatalf("QuestionStatus: %v", err)
	}

	if len(recorded.Options) != 3 || recorded.AgentName != "writer" || recorded.JobName != "release-note" {
		t.Errorf("recorded question = %+v, want the offered options and the asking step's identity", recorded)
	}
}

// TestAskUserTerminalAnswerWins covers the inline-human rung through its seam.
// The prompter stands in for somebody typing, which no test can have — but the
// rest of the path (record the row, report the answer, name the answerer) is
// the real one.
func TestAskUserTerminalAnswerWins(t *testing.T) {
	t.Parallel()

	fixture := newAskFixture(t)
	fixture.env.ask.prompt = func(context.Context, store.Question) (string, bool) {
		return "minor", true
	}

	result := fixture.ask(askGrant{wait: 5 * time.Second}, "Which bump?")

	if result["answer"] != "minor" || result["answered"] != true {
		t.Errorf("terminal ask = %v, want the typed answer", result)
	}

	recorded, err := fixture.store.QuestionStatus(fixture.ctx, 1)
	if err != nil {
		t.Fatalf("QuestionStatus: %v", err)
	}

	if recorded.Status != "answered" || recorded.Answer != "minor" {
		t.Errorf("recorded question = %+v, want the typed answer recorded", recorded)
	}
}

// TestAskUserAnswerFromAnotherProcessEndsTheWait: `steps answer` and the web
// UI write the row, and a parked call is polling it. The row is the answer of
// record; this process is only reading it.
func TestAskUserAnswerFromAnotherProcessEndsTheWait(t *testing.T) {
	t.Parallel()

	fixture := newAskFixture(t)

	go func() {
		for range 100 {
			err := fixture.store.AnswerQuestion(context.Background(), 1, "minor", "jtarchie")
			if err == nil {
				return
			}

			time.Sleep(10 * time.Millisecond)
		}
	}()

	result := fixture.ask(askGrant{wait: 10 * time.Second}, "Which bump?")

	if result["answer"] != "minor" || result["source"] != "jtarchie" {
		t.Errorf("parked ask = %v, want the out-of-band answer and who gave it", result)
	}
}

// TestAskUserAbandonsItsQuestionWhenTheStepEnds: an unanswerable question left
// sitting pending forever in `steps questions` is the same class of lie as a
// default presented as a person's decision.
func TestAskUserAbandonsItsQuestionWhenTheStepEnds(t *testing.T) {
	t.Parallel()

	fixture := newAskFixture(t)

	ctx, cancel := context.WithCancel(fixture.ctx)
	fixture.ctx = ctx

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	result := fixture.ask(askGrant{wait: time.Minute}, "Which bump?")

	if _, isError := result["error"]; !isError {
		t.Errorf("an abandoned question returned %v, want an error result", result)
	}

	pending, err := fixture.store.PendingQuestions(context.Background())
	if err != nil {
		t.Fatalf("PendingQuestions: %v", err)
	}

	if len(pending) != 0 {
		t.Errorf("the abandoned question is still listed as answerable: %+v", pending)
	}
}

// TestAskUserWithoutARunSaysSo: a hook conversation holds no store, and a
// question nothing could ever surface must be reported as data rather than
// parked against a row that does not exist.
func TestAskUserWithoutARunSaysSo(t *testing.T) {
	t.Parallel()

	grant := askGrant{wait: time.Second}

	result := grant.ask(t.Context(), map[string]any{askUserQuestionArg: "Which bump?"}, toolEnv{})

	message, _ := result["error"].(string)
	if !strings.Contains(message, "nobody to ask") {
		t.Errorf("ask without a run = %v, want an explanation the model can act on", result)
	}
}

// TestAskUserRefusesAnEmptyQuestion: the model gets told what is missing, on
// its own next turn, like every other bad-argument case.
func TestAskUserRefusesAnEmptyQuestion(t *testing.T) {
	t.Parallel()

	fixture := newAskFixture(t)

	result := fixture.ask(askGrant{wait: time.Second}, "   ")

	if message, _ := result["error"].(string); !strings.Contains(message, "question is required") {
		t.Errorf("empty question = %v, want a recoverable argument error", result)
	}
}

// TestApplyQuestionBudgetBindsMaxQuestions proves max_questions: reaches the
// machinery that enforces it — the max_calls: counter, which is what makes the
// (N+1)th ask ordinary tool-result data rather than an aborted attempt.
func TestApplyQuestionBudgetBindsMaxQuestions(t *testing.T) {
	t.Parallel()

	tools := agentTools{
		registry: map[string]toolImpl{config.AskUserBuiltinName: nil},
		maxCalls: map[string]int{},
	}

	applyQuestionBudget(tools, config.ResolvedInvocation{MaxQuestions: 3})

	if tools.maxCalls[config.AskUserBuiltinName] != 3 {
		t.Errorf("max_questions bound %d calls, want 3", tools.maxCalls[config.AskUserBuiltinName])
	}

	// 0 is no cap, per the convention every other dial follows.
	uncapped := agentTools{
		registry: map[string]toolImpl{config.AskUserBuiltinName: nil},
		maxCalls: map[string]int{},
	}

	applyQuestionBudget(uncapped, config.ResolvedInvocation{MaxQuestions: 0})

	if _, capped := uncapped.maxCalls[config.AskUserBuiltinName]; capped {
		t.Error("max_questions: 0 bound a budget; 0 means no cap")
	}

	// And a step that cannot ask gets no budget, which is what keeps the dial
	// out of the merkle content of every agent step that never asks anything.
	ungranted := agentTools{registry: map[string]toolImpl{}, maxCalls: map[string]int{}}

	applyQuestionBudget(ungranted, config.ResolvedInvocation{MaxQuestions: 3})

	if len(ungranted.maxCalls) != 0 {
		t.Errorf("a step without an ask_user grant got a question budget: %v", ungranted.maxCalls)
	}
}

// TestParseAnswerSeed: the match may not contain an =, the answer may.
func TestParseAnswerSeed(t *testing.T) {
	t.Parallel()

	seed, err := ParseAnswerSeed("which bump=key=value")
	if err != nil {
		t.Fatalf("ParseAnswerSeed: %v", err)
	}

	if seed.Match != "which bump" || seed.Answer != "key=value" {
		t.Errorf("ParseAnswerSeed = %+v, want the first = to split it", seed)
	}

	for _, bad := range []string{"no separator", "=answer", "match="} {
		_, err := ParseAnswerSeed(bad)
		if err == nil {
			t.Errorf("ParseAnswerSeed(%q) was accepted", bad)
		}
	}
}

// TestAskUserResponderAnswersBeforeAnybodyIsAsked is the escalation rung: a
// bigger model answers, the row says an agent did, and no person is disturbed.
func TestAskUserResponderAnswersBeforeAnybodyIsAsked(t *testing.T) {
	t.Parallel()

	fixture := newAskFixture(t)
	fixture.env.dir = t.TempDir()

	responder := newTestSubAgent(t, &fakeLLM{responses: []*model.LLMResponse{
		{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "  minor  "}}}},
	}})

	grant := impatient("patch")
	grant.answeredBy, grant.responder = "architect", responder.run

	result := fixture.ask(grant, "Which bump?", "major", "minor", "patch")

	if result["answer"] != "minor" || result["answered"] != true {
		t.Errorf("responder ask = %v, want the responder's answer", result)
	}

	// Which channel answered is the thing worth recording: an escalation a
	// person could have intercepted reads differently in an audit than a
	// person's own decision.
	recorded, err := fixture.store.QuestionStatus(fixture.ctx, 1)
	if err != nil {
		t.Fatalf("QuestionStatus: %v", err)
	}

	if recorded.AnsweredBy != "agent:architect" {
		t.Errorf("recorded answered_by = %q, want it to name the responding agent", recorded.AnsweredBy)
	}
}

// TestAskUserResponderFailureFallsThroughToAPerson: the ladder is a ladder. A
// responder that errors, or that answers with nothing, must not resolve the
// question — it hands it on, which here means the declared default.
func TestAskUserResponderFailureFallsThroughToAPerson(t *testing.T) {
	t.Parallel()

	for name, fake := range map[string]*fakeLLM{
		"responder errored": {errs: []error{errors.New("boom")}},
		"responder said nothing": {responses: []*model.LLMResponse{
			{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "   "}}}},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			fixture := newAskFixture(t)
			fixture.env.dir = t.TempDir()

			responder := newTestSubAgent(t, fake)

			grant := impatient("patch")
			grant.answeredBy, grant.responder = "architect", responder.run

			result := fixture.ask(grant, "Which bump?")

			if result["answer"] != "patch" || result["source"] != "default" {
				t.Errorf("ask after a failed responder = %v, want the fall-through default", result)
			}
		})
	}
}

// TestBuildAskUserToolRefusesAWaitItCannotHonour: the deadline on this grant
// is how long a person is waited on, so a value that would silently not bind
// is refused at preparation rather than discovered by somebody waiting.
func TestBuildAskUserToolRefusesAWaitItCannotHonour(t *testing.T) {
	t.Parallel()

	for _, timeout := range []string{"soon", "0", "-5m"} {
		_, _, _, err := buildAskUserTool(t.Context(), nil, config.ToolSpec{
			Builtin: config.AskUserBuiltinName, Timeout: timeout,
		})
		if err == nil {
			t.Errorf("timeout %q was accepted as a wait", timeout)
		}
	}
}

// TestBuildAskUserToolBindsTheGrantsWait: the declaration reaches the model
// under the tool's own name, and the wait the pipeline declared is the one the
// impl uses — not the package default.
func TestBuildAskUserToolBindsTheGrantsWait(t *testing.T) {
	t.Parallel()

	decl, impl, closer, err := buildAskUserTool(t.Context(), nil, config.ToolSpec{
		Builtin: config.AskUserBuiltinName, Timeout: "90s", Default: "patch",
	})
	if err != nil {
		t.Fatalf("buildAskUserTool: %v", err)
	}

	if closer != nil {
		t.Error("a grant with no responder opened something that needs closing")
	}

	if decl.Name != config.AskUserBuiltinName || impl == nil {
		t.Fatalf("declaration = %+v, impl == nil: %v", decl, impl == nil)
	}

	if _, required := decl.Parameters.Properties[askUserQuestionArg]; !required {
		t.Errorf("the declaration offers no %q argument: %+v", askUserQuestionArg, decl.Parameters)
	}

	// A responder needs a config to resolve against; without one the grant
	// cannot be honoured and must not be silently downgraded to "park it".
	_, _, _, err = buildAskUserTool(t.Context(), nil, config.ToolSpec{
		Builtin: config.AskUserBuiltinName, AnsweredBy: "architect",
	})
	if err == nil {
		t.Error("answered_by was accepted with no config to resolve the responder")
	}
}

// TestAskUserWaitReadsTheGrant is what the CLI path uses to size the child's
// own tool-call deadline, and it has to agree with what the impl waits.
func TestAskUserWaitReadsTheGrant(t *testing.T) {
	t.Parallel()

	specs := []config.ToolSpec{{Builtin: "read_file"}, {Builtin: config.AskUserBuiltinName, Timeout: "12m"}}
	if got := askUserWait(specs); got != 12*time.Minute {
		t.Errorf("askUserWait = %s, want 12m", got)
	}

	if got := askUserWait([]config.ToolSpec{{Builtin: config.AskUserBuiltinName}}); got != defaultAskUserWait {
		t.Errorf("askUserWait with no timeout = %s, want the package default", got)
	}
}

// TestAskUserMemoDoesNotReplayAnUnansweredQuestion: the memo returns an
// ANSWER, and an expired-with-no-default row has none. Handing that back as a
// resolved call gave the second asker an empty string labelled as a default
// the pipeline never declared — this design's own audit lie, with nothing in
// it — and left the step running.
func TestAskUserMemoDoesNotReplayAnUnansweredQuestion(t *testing.T) {
	t.Parallel()

	fixture := newAskFixture(t)

	// The first asker gives up with nothing: no default, so it aborts.
	fixture.ask(impatient(""), "Which bump?")

	// A second cell of the same matrix reaches the same row.
	fresh := newAskFixture(t)
	fresh.store, fresh.ctx = fixture.store, fixture.ctx
	fresh.env.ask = fixture.env.ask
	fresh.env.ask.state = &askState{}

	second := fresh.ask(askGrant{wait: time.Minute}, "Which bump?")

	if _, isError := second["error"]; !isError {
		t.Errorf("the second asker got %v, want the first one's ending repeated", second)
	}

	if answer, given := second["answer"]; given && answer != "" {
		t.Errorf("the memo produced an answer nobody gave: %v", answer)
	}

	if fresh.env.ask.state.aborted() == nil {
		t.Error("the second asker carried on from a question that was never answered")
	}
}

// TestAskUserExpiryYieldsToAnAnswerThatJustLanded: the poll sleeps in whole
// intervals, so "the deadline passed" and "nobody answered" are different
// statements. A person who typed at T-0.1s lands while the loop is asleep, and
// expiring on the clock alone would discard the answer the row already holds
// and abort a step somebody just unparked.
func TestAskUserExpiryYieldsToAnAnswerThatJustLanded(t *testing.T) {
	t.Parallel()

	fixture := newAskFixture(t)

	row, _, err := fixture.store.AskQuestion(fixture.ctx, store.Question{
		RunID: "run-1", JobName: "release-note", AgentName: "writer", Question: "Which bump?",
	})
	if err != nil {
		t.Fatalf("AskQuestion: %v", err)
	}

	// Recorded before the expiry runs — the state a slept-through poll wakes to.
	err = fixture.store.AnswerQuestion(fixture.ctx, row.ID, "minor", "jtarchie")
	if err != nil {
		t.Fatalf("AnswerQuestion: %v", err)
	}

	result := impatient("").expire(fixture.ctx, fixture.env, row)

	if result["answer"] != "minor" || result["source"] != "jtarchie" {
		t.Errorf("expire() = %v, want the answer that landed before the deadline", result)
	}

	if fixture.env.ask.state.aborted() != nil {
		t.Error("a question somebody answered aborted the step anyway")
	}
}

// TestAskUserRefusesOptionsRequiredWithNothingOffered: a fence that admits
// nothing is a deadlock, not a fence — every channel would refuse every
// possible answer and the question would run its whole wait out with no
// diagnostic naming the cause.
func TestAskUserRefusesOptionsRequiredWithNothingOffered(t *testing.T) {
	t.Parallel()

	fixture := newAskFixture(t)

	grant := impatient("patch")
	grant.optionsRequired = true

	result := fixture.ask(grant, "Which environment?")

	message, _ := result["error"].(string)
	if !strings.Contains(message, "options list") {
		t.Errorf("ask = %v, want a refusal the model can fix by asking again with options", result)
	}

	pending, err := fixture.store.PendingQuestions(fixture.ctx)
	if err != nil {
		t.Fatalf("PendingQuestions: %v", err)
	}

	if len(pending) != 0 {
		t.Errorf("an unanswerable question was parked for a person anyway: %+v", pending)
	}
}

// TestAskUserKeepsWaitingAfterARefusedTerminalAnswer: an answer the options
// fence rejects leaves the question OPEN. Returning an error there ended the
// wait and stranded the row pending, with the person still sitting at the
// terminal and `steps answer` still able to land.
func TestAskUserKeepsWaitingAfterARefusedTerminalAnswer(t *testing.T) {
	t.Parallel()

	fixture := newAskFixture(t)

	typed := make(chan string, 2)
	typed <- "canary" // not on the list
	typed <- "prod"   // the correction

	fixture.env.ask.prompt = func(ctx context.Context, _ store.Question) (string, bool) {
		select {
		case answer := <-typed:
			return answer, true
		case <-ctx.Done():
			return "", false
		}
	}

	grant := askGrant{wait: 10 * time.Second, optionsRequired: true}

	result := fixture.ask(grant, "Which environment?", "staging", "prod")

	if result["answer"] != "prod" || result["answered"] != true {
		t.Errorf("ask = %v, want the corrected answer after the refused one", result)
	}
}

// TestSubAgentCarriesTheAskContext: a sub-agent granted ask_user asks on
// behalf of the same recorded run. Building its env without the parent's ask
// context told it there was nobody to ask on a run that manifestly had
// somebody — and nothing at load rejected the grant, so the capability
// silently did not exist one level down.
func TestSubAgentCarriesTheAskContext(t *testing.T) {
	t.Parallel()

	parent := newAskFixture(t)
	child := parent.env.ask.forAgent("helper")

	if child.st != parent.env.ask.st || child.jobName != parent.env.ask.jobName {
		t.Error("a sub-agent's ask context lost the run it belongs to")
	}

	if child.agentName != "helper" {
		t.Errorf("a sub-agent's question would be filed under %q, want the child's own name", child.agentName)
	}

	// And it really reaches the tool: asking through the child's context
	// records a row naming the child.
	env := parent.env
	env.ask = child

	grant := impatient("patch")
	grant.ask(parent.ctx, map[string]any{askUserQuestionArg: "Which bump?"}, env)

	recorded, err := parent.store.QuestionStatus(parent.ctx, 1)
	if err != nil {
		t.Fatalf("QuestionStatus: %v", err)
	}

	if recorded.AgentName != "helper" {
		t.Errorf("recorded agent = %q, want the sub-agent that asked", recorded.AgentName)
	}
}
