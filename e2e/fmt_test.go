package e2e

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/docs"
	"github.com/jtarchie/steps/internal/cli"
)

// stepsFmt runs `steps fmt` with stdin and its own streams, returning what it printed to each.
func stepsFmt(t *testing.T, stdin string, args ...string) (string, string, error) {
	t.Helper()

	var stdout, stderr bytes.Buffer

	err := cli.RunWith(cli.Streams{In: strings.NewReader(stdin), Out: &stdout, Err: &stderr}, append([]string{"fmt"}, args...))

	return stdout.String(), stderr.String(), err
}

const unformattedPipeline = `# A pipeline written the way people write them.

resources:
- type: git
  name: repo
  source: {uri: https://example.com/repo.git, branch: main}
- name: board
  type: mock
  source:
    zeta: 1        # free-form: author order stays,
                   # and this line continues the comment above
    alpha: [factory:planning, 'quoted']
jobs:
- plan:
  - run: echo building
    task: build
    outputs: [out]


  # the gate, written once
  - &check
    task: check   # the gate
    run: |
      # a shell comment, not YAML
      test -d out
    inputs: [out]
    on_failure:
      task: say
      run: echo broke
  # the same gate, again
  - <<: *check
    task: recheck
  - task: last
    run: "true"     # one sentence that
    timeout: 1m     # runs across two keys
  - run: echo tail
    task: tail
    # said under the step, about all of it
  - task: end
    run: "true"
  name: build
  assert:
    execution: [build, check, recheck, last, tail, end]
    outcome: succeeded
  on_success:
    task: hurray
    run: echo done
resource_types:
- name: mock
  config:
    check: echo '[]'
    in: "true"
    out: "true"
`

const formattedPipeline = `# A pipeline written the way people write them.

resources:
  - name: repo
    type: git
    source: {uri: https://example.com/repo.git, branch: main}

  - name: board
    type: mock
    source:
      zeta: 1      # free-form: author order stays,
                   # and this line continues the comment above
      alpha: [factory:planning, 'quoted']

jobs:
  - name: build
    plan:
      - task: build
        outputs: [out]
        run: echo building

      # the gate, written once
      - &check
        task: check   # the gate
        inputs: [out]
        run: |
          # a shell comment, not YAML
          test -d out
        on_failure:
          task: say
          run: echo broke
      # the same gate, again
      - task: recheck
        <<: *check
      - task: last
        run: "true" # one sentence that
        timeout: 1m # runs across two keys
      - run: echo tail
        task: tail
        # said under the step, about all of it
      - task: end
        run: "true"
    assert:
      execution: [build, check, recheck, last, tail, end]
      outcome: succeeded
    on_success:
      task: hurray
      run: echo done

resource_types:
  - name: mock
    config:
      check: echo '[]'
      in: "true"
      out: "true"
`

// TestFmtRewritesAPipelineIntoTheStandardLayout is the command a person runs on a file: what it is first, settings, body, assert and hooks last; lists indented under their key; blank lines by structure, the author's single one kept and a run collapsed; and every comment, scalar spelling, inline list, anchor and merge as written.
func TestFmtRewritesAPipelineIntoTheStandardLayout(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "pipeline.yml")
	writePipelineFile(t, path, unformattedPipeline)

	stdout, _, err := stepsFmt(t, "", path)
	if err != nil {
		t.Fatalf("steps fmt: %v", err)
	}

	if strings.TrimSpace(stdout) != path {
		t.Errorf("steps fmt printed %q, want the path it rewrote", stdout)
	}

	got, err := os.ReadFile(path) //nolint:gosec // a temp file this test wrote
	if err != nil {
		t.Fatal(err)
	}

	if string(got) != formattedPipeline {
		t.Errorf("formatted file:\n%s\nwant:\n%s", got, formattedPipeline)
	}

	stdout, _, err = stepsFmt(t, "", "--check", path)
	if err != nil || stdout != "" {
		t.Errorf("a second pass is not a no-op: printed %q, err %v", stdout, err)
	}
}

// TestFmtCheckAndDiffChangeNothing: --check fails naming the file, --diff shows the change, and neither writes.
func TestFmtCheckAndDiffChangeNothing(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "pipeline.yml")
	writePipelineFile(t, path, unformattedPipeline)

	stdout, _, err := stepsFmt(t, "", "--check", path)
	if err == nil || !strings.Contains(err.Error(), "1 file(s) not formatted") || strings.TrimSpace(stdout) != path {
		t.Errorf("--check on an unformatted file: printed %q, err %v; want the path and a failure", stdout, err)
	}

	stdout, _, err = stepsFmt(t, "", "--diff", path)
	if err != nil {
		t.Fatalf("--diff: %v", err)
	}

	if !strings.Contains(stdout, "-- name: board\n") || !strings.Contains(stdout, "+  - name: board\n") {
		t.Errorf("--diff printed no unified diff of the change:\n%s", stdout)
	}

	got, err := os.ReadFile(path) //nolint:gosec // a temp file this test wrote
	if err != nil {
		t.Fatal(err)
	}

	if string(got) != unformattedPipeline {
		t.Errorf("--check/--diff wrote the file:\n%s", got)
	}
}

// TestFmtFormatsStandardInputToStandardOutput is the editor integration: `-` reads the pipeline and prints it formatted, ((var)) placeholders and all, since nothing is substituted.
func TestFmtFormatsStandardInputToStandardOutput(t *testing.T) {
	t.Parallel()

	withVar := strings.Replace(unformattedPipeline, "branch: main", "branch: ((branch))", 1)

	stdout, _, err := stepsFmt(t, withVar, "-")
	if err != nil {
		t.Fatalf("steps fmt -: %v", err)
	}

	want := strings.Replace(formattedPipeline, "branch: main", "branch: ((branch))", 1)
	if stdout != want {
		t.Errorf("steps fmt - printed:\n%s\nwant:\n%s", stdout, want)
	}
}

// TestFmtDirectorySkipsWhatIsNotAPipeline: a folder holds vars files and task files beside pipelines, and fmt names each it passed over rather than refusing or rewriting it.
func TestFmtDirectorySkipsWhatIsNotAPipeline(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	pipeline := filepath.Join(dir, "ci", "pipeline.yml")
	vars := filepath.Join(dir, "ci", "vars.yml")

	err := os.MkdirAll(filepath.Dir(pipeline), 0o750)
	if err != nil {
		t.Fatal(err)
	}

	writePipelineFile(t, pipeline, unformattedPipeline)

	const varsFile = "branch:   main\nlist:\n- a\n"
	writePipelineFile(t, vars, varsFile)

	_, stderr, err := stepsFmt(t, "", dir)
	if err != nil {
		t.Fatalf("steps fmt %s: %v", dir, err)
	}

	if !strings.Contains(stderr, "skipped "+vars+": not a pipeline") {
		t.Errorf("stderr does not name the skipped vars file:\n%s", stderr)
	}

	got, _ := os.ReadFile(pipeline) //nolint:gosec // a temp file this test wrote
	if string(got) != formattedPipeline {
		t.Errorf("the pipeline in the folder was not formatted:\n%s", got)
	}

	got, _ = os.ReadFile(vars) //nolint:gosec // a temp file this test wrote
	if string(got) != varsFile {
		t.Errorf("the vars file was rewritten:\n%s", got)
	}
}

// TestFmtRefusesAndLeavesTheFileAlone: a file the guard cannot prove unchanged in meaning is not written, and the error says why.
func TestFmtRefusesAndLeavesTheFileAlone(t *testing.T) {
	t.Parallel()

	const duplicated = "jobs:\n- name: a\n  name: b\n  plan:\n  - task: t\n    run: \"true\"\n"

	path := filepath.Join(t.TempDir(), "pipeline.yml")
	writePipelineFile(t, path, duplicated)

	_, _, err := stepsFmt(t, "", path)
	if err == nil || !strings.Contains(err.Error(), "refusing to format") || !strings.Contains(err.Error(), `"name" already defined`) {
		t.Fatalf("err = %v, want a refusal naming the problem", err)
	}

	got, _ := os.ReadFile(path) //nolint:gosec // a temp file this test wrote
	if string(got) != duplicated {
		t.Errorf("a refused file was rewritten:\n%s", got)
	}
}

// TestShippedPipelinesAreFormatted holds the docs to being the style reference: every pipeline block in docs/ and every file in examples/ is already what steps fmt writes.
func TestShippedPipelinesAreFormatted(t *testing.T) {
	t.Parallel()

	stdout, _, err := stepsFmt(t, "", "--check", repoFile("examples"))
	if err != nil {
		t.Errorf("examples/ is not formatted (run steps fmt examples): %v\n%s", err, stdout)
	}

	blocks, err := docs.Blocks()
	if err != nil {
		t.Fatal(err)
	}

	for _, block := range blocks {
		if mode := block.Mode(); mode != "run" && mode != "noexec" {
			continue
		}

		formatted, _, err := stepsFmt(t, block.Body, "-")
		if err != nil {
			t.Errorf("%s: %v", block.Name(), err)

			continue
		}

		if formatted != block.Body {
			t.Errorf("%s is not formatted; steps fmt would write:\n%s", block.Name(), formatted)
		}
	}
}
