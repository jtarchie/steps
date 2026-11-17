package e2e

// steps#272 parts 3 to 5: values a run captures are read as ((.:name)), as in
// Concourse — a load_var:'s, and a get's facts — load_var: reads json and yaml,
// and a captured value is hidden in step output unless the step reveals it.

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// factsReaderPipeline is factsPipeline's ticket type, with a task that reads the get's facts and keeps an output, so the step cache could otherwise reuse it.
func factsReaderPipeline(log string) string {
	return fmt.Sprintf(`
resource_types:
- name: ticket
  config:
    check: |
      printf '[{"id": "412"}]'
    in: |
      printf '{"version": {"id": "412"}, "metadata": [{"name": "url", "value": "https://tickets.example/412"}, {"name": "title", "value": "Add the widget cache"}]}'
resources:
- name: pr
  type: ticket
  source: {}
jobs:
- name: answer
  plan:
  - get: pr
    trigger: true
  - task: write
    outputs: [answer]
    run: |
      echo "Reviewed ((.:pr.url)) (((.:pr.title)))" > answer/comment.md
      cat answer/comment.md
      cat answer/comment.md >> %s
`, log)
}

func TestEndToEndALaterStepReadsAGetsFacts(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	log := filepath.Join(dir, "comments.log")
	path := writePipeline(t, dir, factsReaderPipeline(log))
	root := filepath.Join(dir, "root")

	out := runCaptured(t, "run", path, "--job", "answer", "--progress", "plain", "--workspace-root", root)

	if got := readFileString(t, log); got != "Reviewed https://tickets.example/412 (Add the widget cache)\n" {
		t.Fatalf("the task wrote %q, want the get's facts substituted", got)
	}

	// A get's facts are never hidden: they are what the resource chose to show.
	if !strings.Contains(out, "Reviewed https://tickets.example/412") {
		t.Errorf("a get's facts were masked in step output:\n%s", out)
	}

	// The same version, the same inputs, the same text: everything a cache keys on is unchanged, and the step still runs, because its facts are only known once the run is under way.
	again := runCaptured(t, "run", path, "--job", "answer", "--progress", "plain", "--workspace-root", root)

	if got := readFileString(t, log); strings.Count(got, "Reviewed") != 2 {
		t.Fatalf("over two runs the task ran %d times, want twice — a step reading a get's facts always re-runs\n%s", strings.Count(got, "Reviewed"), again)
	}

	if !strings.Contains(again, "reads ((.:pr.url))") {
		t.Errorf("the run does not say why the chain is never skipped:\n%s", again)
	}
}

// TestEndToEndAMatrixReadingAGetsFactsAlwaysRuns: a matrix cell is cached on its own hash, which the facts are part of, and still runs.
func TestEndToEndAMatrixReadingAGetsFactsAlwaysRuns(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	log := filepath.Join(dir, "cells.log")
	path := writePipeline(t, dir, strings.Replace(factsReaderPipeline(log), `  - task: write
    outputs: [answer]
    run: |
      echo "Reviewed ((.:pr.url)) (((.:pr.title)))" > answer/comment.md
      cat answer/comment.md
      cat answer/comment.md >> `+log+`
`, `  - task: cell
    run: echo "{{ .vars.dim }} ((.:pr.url))" >> `+log+`
    across:
    - var: dim
      values: [api, ui]
`, 1))

	root := filepath.Join(dir, "root")

	runCaptured(t, "run", path, "--job", "answer", "--progress", "plain", "--workspace-root", root)
	runCaptured(t, "run", path, "--job", "answer", "--progress", "plain", "--workspace-root", root)

	if got := readFileString(t, log); strings.Count(got, "api https://tickets.example/412") != 2 || strings.Count(got, "ui https://tickets.example/412") != 2 {
		t.Errorf("over two runs the cells wrote %q, want each cell twice", got)
	}
}

// TestValidateWarnsWhenAGetsFactReachesTheShell: a pull request's title is its author's text, and run: is sh -c, so a fact pasted there is shell syntax somebody outside the pipeline wrote. A warning, as for an unquoted {{ }}.
func TestValidateWarnsWhenAGetsFactReachesTheShell(t *testing.T) {
	t.Parallel()

	stderr := captureStderr(t)
	path := writePipeline(t, t.TempDir(), factsReaderPipeline("/dev/null")+`  - load_var: tag
    inputs: [answer]
    file: answer/comment.md
  - task: tell
    run: echo ((.:tag))
`)

	err := stepsRun(t, []string{"validate", path, "--syntax-only"})
	if err != nil {
		t.Fatalf("a warning failed validate: %v", err)
	}

	got := stderr()
	if !strings.Contains(got, `warning: job "answer" step 1`) || !strings.Contains(got, `run: ((.:pr.title)) is a fact get "pr" reported`) {
		t.Errorf("no warning for a fact in run:\n%s", got)
	}

	if strings.Contains(got, "((.:tag))") {
		t.Errorf("a load_var: value is the pipeline's own, not warned on:\n%s", got)
	}
}

func TestEndToEndAMissingFactFailsTheStep(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := writePipeline(t, dir, strings.Replace(factsReaderPipeline(filepath.Join(dir, "log")), "((.:pr.title))", "((.:pr.author))", 1))

	var runErr error

	out := captureStdout(t, func() { runErr = stepsRun(t, []string{"run", path, "--job", "answer", "--progress", "plain"}) })
	if runErr == nil {
		t.Fatalf("a step reading a fact the get did not report succeeded:\n%s", out)
	}

	if !strings.Contains(runErr.Error(), "((.:pr.author))") || !strings.Contains(runErr.Error(), `"author"`) {
		t.Errorf("the error does not name the reference and the missing fact: %v", runErr)
	}
}

func TestEndToEndAPutsFactsAreNotReadable(t *testing.T) {
	t.Parallel()

	path := writePipeline(t, t.TempDir(), strings.Replace(factsPipeline, "  - put: review\n",
		"  - put: review\n  - task: after\n    run: echo ((.:review.url))\n", 1))

	err := stepsRun(t, []string{"validate", path})
	if err == nil {
		t.Fatal("a step reading a put's facts validated")
	}

	if !strings.Contains(err.Error(), "((.:review.url))") || !strings.Contains(err.Error(), "put") {
		t.Errorf("the error does not say a put's facts are not available: %v", err)
	}
}

// loadVarPipeline captures meta/<file> after a task writes body to it, then echoes use.
func loadVarPipeline(file, body, loadVarExtra, use string) string {
	return fmt.Sprintf(`
jobs:
- name: build
  plan:
  - task: write
    outputs: [meta]
    run: |
      printf '%s' > meta/%s
  - load_var: info
    inputs: [meta]
    file: meta/%s%s
  - task: use
    run: |
      echo "[%s]"
`, body, file, file, loadVarExtra, use)
}

// runOut is a plain run's output, and its error.
func runOut(t *testing.T, path string) (string, error) {
	t.Helper()

	var err error

	out := captureStdout(t, func() { err = stepsRun(t, []string{"run", path, "--job", "build", "--progress", "plain"}) })

	return out, err
}

func TestEndToEndLoadVarReadsTheFileByItsExtension(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, file, body, extra, use, want string
	}{
		{"json fields", "info.json", `{"version": "1.2.3", "build": 7, "rc": true}`, "", "((.:info.version)) ((.:info.build)) ((.:info.rc))", "[1.2.3 7 true]"},
		{"yaml fields", "info.yml", `version: 2.0.0\nnested:\n  name: blue\n`, "", "((.:info.version)) ((.:info.nested.name))", "[2.0.0 blue]"},
		{"yaml extension", "info.yaml", `version: 3.0.0\n`, "", "((.:info.version))", "[3.0.0]"},
		{"anything else is trimmed", "version.txt", `v1.2.3\n`, "", "((.:info))", "[v1.2.3]"},
		{"format: trim keeps the old behaviour", "info.json", `  {a: 1}\n`, "\n    format: trim", "((.:info))", "[{a: 1}]"},
		{"format: raw keeps the newline", "version.txt", `v1\n`, "\n    format: raw", "((.:info))", "[v1\n[use] ]"},
		{"format: json beats the extension", "info.txt", `{"v": "x9"}`, "\n    format: json", "((.:info.v))", "[x9]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := writePipeline(t, t.TempDir(), loadVarPipeline(tc.file, tc.body, tc.extra+"\n    reveal: true", tc.use))

			out, err := runOut(t, path)
			if err != nil {
				t.Fatalf("run: %v\n%s", err, out)
			}

			if !strings.Contains(out, tc.want) {
				t.Errorf("output does not hold %q:\n%s", tc.want, out)
			}
		})
	}
}

func TestEndToEndAFieldOfAPlainValueFailsTheStep(t *testing.T) {
	t.Parallel()

	path := writePipeline(t, t.TempDir(), loadVarPipeline("version.txt", `v1`, "\n    reveal: true", "((.:info.major))"))

	out, err := runOut(t, path)
	if err == nil {
		t.Fatalf("a field of a trimmed value rendered:\n%s", out)
	}

	if !strings.Contains(err.Error(), "((.:info.major))") {
		t.Errorf("the error does not name the reference: %v", err)
	}
}

func TestEndToEndACapturedValueIsHiddenUnlessRevealed(t *testing.T) {
	t.Parallel()

	hidden, err := runOut(t, writePipeline(t, t.TempDir(), loadVarPipeline("token.txt", `s3cr3t-value\n`, "", "token ((.:info)) ok")))
	if err != nil {
		t.Fatalf("run: %v\n%s", err, hidden)
	}

	if strings.Contains(hidden, "s3cr3t-value") || !strings.Contains(hidden, "[token *** ok]") {
		t.Errorf("a captured value was shown in step output:\n%s", hidden)
	}

	structured, err := runOut(t, writePipeline(t, t.TempDir(), loadVarPipeline("creds.json", `{"user": "deployer-bot", "port": 5432}`, "", "((.:info.user)) ((.:info.port))")))
	if err != nil {
		t.Fatalf("run: %v\n%s", err, structured)
	}

	// Each string of a structured value is hidden; a number is not a string, as in Concourse.
	if strings.Contains(structured, "deployer-bot") || !strings.Contains(structured, "[*** 5432]") {
		t.Errorf("a structured value's string was shown in step output:\n%s", structured)
	}

	shown, err := runOut(t, writePipeline(t, t.TempDir(), loadVarPipeline("token.txt", `s3cr3t-value\n`, "\n    reveal: true", "token ((.:info)) ok")))
	if err != nil {
		t.Fatalf("run: %v\n%s", err, shown)
	}

	if !strings.Contains(shown, "[token s3cr3t-value ok]") {
		t.Errorf("reveal: true did not show the value:\n%s", shown)
	}
}

// TestEndToEndAHiddenValueIsNotKeptInTheDatabase: the value is computed by the run, so the pipeline's own text never holds it, and nothing the run records may.
func TestEndToEndAHiddenValueIsNotKeptInTheDatabase(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	state := filepath.Join(dir, "state.db")
	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - task: write
    outputs: [meta]
    run: echo "hidden-$((6*7))-token" > meta/token.txt
  - load_var: token
    inputs: [meta]
    file: meta/token.txt
  - task: use
    run: echo "using ((.:token))"
`)

	err := stepsRun(t, []string{"run", "--db", state, path, "--job", "build"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	assertNotInDatabase(t, state, "hidden-42-token")
}

// TestEndToEndAMatrixDoesNotReadACapturedValueAsATemplate: a cell's {{ .vars }} pass runs before the capture is filled in, so a fact holding template text stays text.
func TestEndToEndAMatrixDoesNotReadACapturedValueAsATemplate(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	log := filepath.Join(dir, "cells.log")
	pipeline := strings.Replace(factsReaderPipeline(log), `"Add the widget cache"`, `"{{ "{{ .vars.dim }}" }}"`, 1)
	pipeline = strings.Replace(pipeline, `  - task: write
    outputs: [answer]
    run: |
      echo "Reviewed ((.:pr.url)) (((.:pr.title)))" > answer/comment.md
      cat answer/comment.md
      cat answer/comment.md >> `+log+`
`, `  - task: cell
    run: echo '{{ .vars.dim }} ((.:pr.title))' >> `+log+`
    across:
    - var: dim
      values: [api]
`, 1)

	runCaptured(t, "run", writePipeline(t, dir, pipeline), "--job", "answer", "--progress", "plain")

	if got := readFileString(t, log); got != "api {{ .vars.dim }}\n" {
		t.Errorf("the cell wrote %q, want the fact as its text", got)
	}
}

// TestEndToEndAHiddenStringIsMaskedInsideAWholeValue: ((.:creds)) is substituted as JSON, which spells a tab as \t, so the mask of the raw string alone missed it.
func TestEndToEndAHiddenStringIsMaskedInsideAWholeValue(t *testing.T) {
	t.Parallel()

	out, err := runOut(t, writePipeline(t, t.TempDir(), `
jobs:
- name: build
  plan:
  - task: write
    outputs: [meta]
    run: |
      printf '%s' '{"pw": "a\tb-secret-value"}' > meta/creds.json
  - load_var: creds
    inputs: [meta]
    file: meta/creds.json
  - task: use
    run: printf '%s\n' '((.:creds))'
`))
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}

	if strings.Contains(out, "b-secret-value") || !strings.Contains(out, `{"pw":"***"}`) {
		t.Errorf("a hidden string was shown inside the whole value:\n%s", out)
	}
}

func TestEndToEndTheOldSpellingOfACaptureIsRefused(t *testing.T) {
	t.Parallel()

	path := writePipeline(t, t.TempDir(), loadVarPipeline("version.txt", `v1`, "", "((info))"))

	err := stepsRun(t, []string{"validate", path})
	if err == nil {
		t.Fatal("((info)) after load_var: info validated")
	}

	if !strings.Contains(err.Error(), "((.:info))") {
		t.Errorf("the error does not name the new spelling: %v", err)
	}

	// --var still fills ((name)): only a capture moved.
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	flagged := writePipeline(t, dir, fmt.Sprintf("jobs:\n- name: build\n  plan:\n  - task: use\n    run: echo ((info)) > %s\n", log))

	err = stepsRun(t, []string{"run", flagged, "--job", "build", "--var", "info=from-flag"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if got := readFileString(t, log); got != "from-flag\n" {
		t.Errorf("--var wrote %q", got)
	}
}

func TestEndToEndDuplicateLoadVarNamesAreRefused(t *testing.T) {
	t.Parallel()

	path := writePipeline(t, t.TempDir(), strings.Replace(loadVarPipeline("version.txt", `v1`, "", "((.:info))"),
		"  - task: use\n", "  - load_var: info\n    inputs: [meta]\n    file: meta/version.txt\n  - task: use\n", 1))

	err := stepsRun(t, []string{"validate", path})
	if err == nil {
		t.Fatal("two load_var: info in one plan validated")
	}

	if !strings.Contains(err.Error(), `load_var "info"`) || !strings.Contains(err.Error(), "already") {
		t.Errorf("the error does not name the duplicate: %v", err)
	}
}

// TestEndToEndAnAcrossCellsCaptureEndsWithTheMatrix: each cell's captures are its own, so nothing after the matrix can read one.
func TestEndToEndAnAcrossCellsCaptureEndsWithTheMatrix(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	log := filepath.Join(dir, "cells.log")
	matrix := fmt.Sprintf(`
jobs:
- name: build
  plan:
  - task: write
    outputs: [meta]
    run: printf v7 > meta/v.txt
  - do:
    - load_var: v
      inputs: [meta]
      file: meta/v.txt
      reveal: true
    - task: use
      run: echo "((.:v))" >> %s
    across:
    - var: cell
      values: [a, b]
`, log)

	err := stepsRun(t, []string{"run", writePipeline(t, dir, matrix), "--job", "build"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if got := readFileString(t, log); got != "v7\nv7\n" {
		t.Errorf("cells read %q, want each cell to read its own capture", got)
	}

	after := writePipeline(t, t.TempDir(), matrix+"  - task: after\n    run: echo ((.:v))\n")

	err = stepsRun(t, []string{"validate", after})
	if err == nil || !strings.Contains(err.Error(), "((.:v))") {
		t.Errorf("a capture made inside an across: cell was readable after the matrix: %v", err)
	}
}

func TestEndToEndAnUnproducedCaptureIsALoadError(t *testing.T) {
	t.Parallel()

	path := writePipeline(t, t.TempDir(), "jobs:\n- name: build\n  plan:\n  - task: use\n    run: echo ((.:nobody))\n")

	err := stepsRun(t, []string{"validate", path})
	if err == nil || !strings.Contains(err.Error(), "((.:nobody))") {
		t.Fatalf("an unproduced ((.:nobody)) validated: %v", err)
	}
}

// TestEndToEndAHooksTryBodyReadsACapture: a try: in a hook wraps the step that carries the fields, so rendering the hook's own fields reached nothing and the literal ((.:pr.title)) reached the shell.
func TestEndToEndAHooksTryBodyReadsACapture(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	log := filepath.Join(dir, "hook.log")
	path := writePipeline(t, dir, strings.Replace(factsReaderPipeline(filepath.Join(dir, "comments.log")), "      cat answer/comment.md >> "+filepath.Join(dir, "comments.log")+"\n",
		"      cat answer/comment.md >> "+filepath.Join(dir, "comments.log")+"\n    ensure:\n      try:\n        task: note\n        run: echo \"hook ((.:pr.title))\" >> "+log+"\n", 1))

	runCaptured(t, "run", path, "--job", "answer", "--progress", "plain", "--workspace-root", filepath.Join(dir, "root"))

	if got := readFileString(t, log); got != "hook Add the widget cache\n" {
		t.Errorf("the hook's try body wrote %q, want the get's fact substituted", got)
	}
}
