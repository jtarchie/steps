package e2e

import (
	"bufio"
	"fmt"
	"html"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/cli"
	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/store/sqlite"
	"github.com/jtarchie/steps/internal/web"
)

// The parked line spelled a retired grammar and, for a local run, named the daemon's database instead of the run's; readArgs' --db hid both from every other test.
func TestParkedApprovalPrintsACommandThatDecidesIt(t *testing.T) {
	for _, verb := range [][]string{{"run", "--job", "publish"}, {"test"}} {
		t.Run(verb[0], func(t *testing.T) {
			dir := t.TempDir()
			path := approvalPipeline(t, dir, "30s")

			// From the pipeline's directory by its relative name, as a person runs it, so the state path the line must name is relative too.
			t.Chdir(dir)

			lines := watchStdout(t)

			done := make(chan error, 1)

			go func() { done <- cli.Run(append([]string{verb[0], filepath.Base(path)}, verb[1:]...)) }()

			approve, reject, found := strings.Cut(printedCommand(t, lines, "approval "), "  |  ")
			if !found {
				t.Fatalf("the approval line offers no reject command: %q", approve)
			}

			err := runPrinted(approve, "")
			if err != nil {
				t.Fatalf("the printed approve command failed: %v", err)
			}

			err = <-done
			if err != nil {
				t.Fatalf("the run did not continue after the printed command approved it: %v", err)
			}

			assertLineCount(t, filepath.Join(dir, "publish.log"), 1)

			// Only a command that parsed and opened the run's own database can be refused as already decided.
			err = runPrinted(reject, "")
			if err == nil || !strings.Contains(err.Error(), "could not record the decision") {
				t.Errorf("the printed reject command = %v, want it refused as already decided", err)
			}
		})
	}
}

// Same promise for a parked ask_user question, whose line leaves only the answer to the reader.
func TestParkedQuestionPrintsACommandThatAnswersIt(t *testing.T) {
	dir := t.TempDir()

	fake := newRoutedFakeLLM(t, func(req capturedRequest) turn {
		if !req.historyCalled("ask_user") {
			return callsTool("ask_user", map[string]any{"question": "Which environment?"})
		}

		return says("Deploying to " + answeredValue(req.toolResults()) + ".")
	})

	path := writePipeline(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: deployer
  source:
    endpoint: %s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools:
  - builtin: ask_user
    timeout: 30s

jobs:
- name: deploy
  plan:
  - agent: deployer
    messages:
      - Ask which environment, then say where you are deploying.
`, fake.URL))

	t.Chdir(dir)

	lines := watchStdout(t)

	done := make(chan error, 1)

	go func() { done <- cli.Run([]string{"run", filepath.Base(path), "--job", "deploy"}) }()

	err := runPrinted(printedCommand(t, lines, "question "), "staging")
	if err != nil {
		t.Fatalf("the printed answer command failed: %v", err)
	}

	err = <-done
	if err != nil {
		t.Fatalf("the run did not continue after the printed command answered it: %v", err)
	}

	answered := storeQuestions(t, path)
	if len(answered) != 1 || answered[0].Status != "answered" || answered[0].Answer != "staging" {
		t.Errorf("question row = %+v, want the printed command's answer recorded", answered)
	}
}

// A daemon on any --db but the default parks the same line, and without that --db in it the command opens the default database and finds nothing.
func TestParkedApprovalUnderADaemonNamesItsDatabase(t *testing.T) {
	dir := t.TempDir()
	path := approvalPipeline(t, dir, "30s")
	name := cli.PipelineName(path)

	lines := watchStdout(t)

	served := startWebFor(t, path, "--interval", "1h")
	defer served.stopIfRunning(t)

	served.trigger(t, name, "publish")

	approve, _, found := strings.Cut(printedCommand(t, lines, "approval "), "  |  ")
	if !found {
		t.Fatalf("the approval line offers no reject command: %q", approve)
	}

	err := runPrinted(approve, "")
	if err != nil {
		t.Fatalf("the printed approve command failed: %v", err)
	}

	waitForQueueSuccess(t, served.state, name, 1)
	assertLineCount(t, filepath.Join(dir, "publish.log"), 1)
}

// --read-only swaps each control for a command, which must work as printed against a daemon's default database; both pages spelled the retired grammar.
func TestReadOnlyPagesPrintCommandsThatWork(t *testing.T) {
	dir := t.TempDir()
	path := approvalPipeline(t, dir, "30s")
	name := cli.PipelineName(path)

	t.Chdir(dir)

	server, st := readOnlyDaemon(t, path, name)
	ctx := t.Context()

	approval, err := st.RequestApproval(ctx, "publish", "Ship it?")
	if err != nil {
		t.Fatalf("RequestApproval: %v", err)
	}

	question := parkQuestion(t, st, dir)

	err = runPrinted(readOnlyHint(t, server, "/p/"+name+"/approvals"), "")
	if err != nil {
		t.Fatalf("the approvals page's command failed: %v", err)
	}

	err = runPrinted(readOnlyHint(t, server, "/p/"+name+"/questions"), "staging")
	if err != nil {
		t.Fatalf("the questions page's command failed: %v", err)
	}

	decided, err := st.ApprovalStatus(ctx, approval)
	if err != nil || decided.Status != "approved" {
		t.Errorf("approval = %+v (%v), want the page's command to have approved it", decided, err)
	}

	answered, err := st.QuestionStatus(ctx, question)
	if err != nil || answered.Status != "answered" || answered.Answer != "staging" {
		t.Errorf("question = %+v (%v), want the page's command to have answered it", answered, err)
	}
}

// On the default database because the pages' commands name no --db: they reach a daemon from the directory it serves in.
func readOnlyDaemon(t *testing.T, path, name string) (*web.Server, store.Store) {
	t.Helper()

	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	st, err := sqlite.OpenStore(cli.DefaultDaemonState, name)
	if err != nil {
		t.Fatalf("open the daemon's state: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })

	server, err := web.New([]*web.Pipeline{web.NewPipeline(name, path, cfg, st, events.New(nil))}, nil)
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}

	return server, st
}

func parkQuestion(t *testing.T, st store.Store, dir string) int64 {
	t.Helper()

	err := st.StartRun(t.Context(), "PARKEDRUN", "publish", dir, "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	question, _, err := st.AskQuestion(t.Context(), store.Question{
		RunID: "PARKEDRUN", JobName: "publish", AgentName: "writer", Question: "Which environment?",
	})
	if err != nil {
		t.Fatalf("AskQuestion: %v", err)
	}

	return question.ID
}

var readOnlyHintPattern = regexp.MustCompile(`This server is read-only\. \w+ with <code>([^<]+)</code>`)

func readOnlyHint(t *testing.T, server *web.Server, target string) string {
	t.Helper()

	code, body := webGet(t, server, target)
	if code != http.StatusOK {
		t.Fatalf("GET %s = %d", target, code)
	}

	match := readOnlyHintPattern.FindStringSubmatch(body)
	if match == nil {
		t.Fatalf("%s names no command to use instead:\n%s", target, body)
	}

	return html.UnescapeString(match[1])
}

// captureStdout returns only once its command does, and a parked run must be acted on mid-print. Not t.Parallel(): it swaps the process-wide os.Stdout.
func watchStdout(t *testing.T) <-chan string {
	t.Helper()

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}

	orig := os.Stdout
	os.Stdout = writer

	lines := make(chan string, 256)
	drained := make(chan struct{})

	go func() {
		defer close(drained)

		buffered := bufio.NewReader(reader)

		for {
			line, readErr := buffered.ReadString('\n')
			if line != "" {
				select {
				case lines <- strings.TrimRight(line, "\n"):
				default: // the command printing it must never block on a line nobody is waiting for
				}
			}

			if readErr != nil {
				return
			}
		}
	}()

	t.Cleanup(func() {
		os.Stdout = orig
		_ = writer.Close()

		<-drained

		_ = reader.Close()
	})

	return lines
}

func printedCommand(t *testing.T, lines <-chan string, prefix string) string {
	t.Helper()

	deadline := time.After(30 * time.Second)

	for {
		select {
		case line := <-lines:
			if !strings.HasPrefix(line, prefix) || !strings.Contains(line, "waiting up to") {
				continue
			}

			_, commands, found := strings.Cut(line, " — ")
			if !found {
				t.Fatalf("the parked line offers no command: %q", line)
			}

			return commands
		case <-deadline:
			t.Fatalf("nothing printed a parked %q line", prefix)
		}
	}
}

// The <answer> placeholder is the only part a person pasting the line fills in.
func runPrinted(printed, answer string) error {
	fields := strings.Fields(printed)
	if len(fields) == 0 || fields[0] != "steps" {
		return fmt.Errorf("%q is not a steps command", printed)
	}

	args := fields[1:]

	for i, field := range args {
		if field == "<answer>" {
			args[i] = answer
		}
	}

	err := cli.Run(args)
	if err != nil {
		return fmt.Errorf("%s: %w", printed, err)
	}

	return nil
}
