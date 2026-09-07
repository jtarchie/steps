package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestBundleLoadEqualsDirLoad is the load-bearing seam test for #104: a
// daemon resolving a `steps pipeline set` upload through a Bundle must
// produce the exact same revision SHA, Includes list, and resolved
// run:/system: text as loading the same tree off disk through DirFS. A
// one-byte divergence here makes every legitimate `set` fail its
// compare-and-set check against what the CLI diffed locally.
func TestBundleLoadEqualsDirLoad(t *testing.T) {
	t.Parallel()

	source := `
tasks:
- name: unit
  run_file: ci/unit.sh
jobs:
- name: build
  plan:
  - task: unit
`
	script := "echo from-file\n"

	dirPath := writeConfig(t, source)
	writeSibling(t, dirPath, "ci/unit.sh", script)

	dirCfg, err := LoadFS(DirFS(filepath.Dir(dirPath)), filepath.Base(dirPath), "app", nil)
	if err != nil {
		t.Fatalf("LoadFS(DirFS): %v", err)
	}

	bundle := Bundle{
		"pipeline.yml": source,
		"ci/unit.sh":   script,
	}

	bundleCfg, err := LoadFS(bundle, "pipeline.yml", "app", nil)
	if err != nil {
		t.Fatalf("LoadFS(Bundle): %v", err)
	}

	if dirCfg.Revision.SHA != bundleCfg.Revision.SHA {
		t.Fatalf("SHA mismatch: dir=%q bundle=%q", dirCfg.Revision.SHA, bundleCfg.Revision.SHA)
	}

	if !reflect.DeepEqual(dirCfg.Revision.Includes, bundleCfg.Revision.Includes) {
		t.Fatalf("Includes mismatch: dir=%v bundle=%v", dirCfg.Revision.Includes, bundleCfg.Revision.Includes)
	}

	if dirCfg.Tasks[0].Run != bundleCfg.Tasks[0].Run {
		t.Fatalf("resolved Run mismatch: dir=%q bundle=%q", dirCfg.Tasks[0].Run, bundleCfg.Tasks[0].Run)
	}
}

// TestBundleResolvesIncludesAsTheYAMLWritesThem is the same seam a step
// further out, and it is where a map differs from a directory: an include the
// pipeline writes UNCLEANED ("./ci/unit.sh") is read off disk by a
// filepath.Join that cleans it silently, and recorded in the revision as
// "ci/unit.sh". A sender has only Revision.Includes to build its bundle keys
// from, so an exact-match lookup would refuse an include that WAS uploaded —
// and refuse every `set` of that pipeline forever, with a "no such file" that
// names a path the daemon was handed.
func TestBundleResolvesIncludesAsTheYAMLWritesThem(t *testing.T) {
	t.Parallel()

	source := `
tasks:
- name: unit
  run_file: ./ci/unit.sh
jobs:
- name: build
  plan:
  - task: unit
`
	script := "echo from-file\n"

	dirPath := writeConfig(t, source)
	writeSibling(t, dirPath, "ci/unit.sh", script)

	dirCfg, err := LoadConfig(dirPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if got, want := dirCfg.Revision.Includes, []string{"ci/unit.sh"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Includes = %v, want %v — the bundle key a sender would build", got, want)
	}

	// Keyed exactly as a sender builds it: off Revision.Includes above.
	bundle := Bundle{"pipeline.yml": source}
	for _, include := range dirCfg.Revision.Includes {
		bundle[include] = script
	}

	bundleCfg, err := LoadFS(bundle, "pipeline.yml", "app", nil)
	if err != nil {
		t.Fatalf("LoadFS(Bundle): %v", err)
	}

	if dirCfg.Revision.SHA != bundleCfg.Revision.SHA {
		t.Errorf("SHA mismatch: dir=%q bundle=%q", dirCfg.Revision.SHA, bundleCfg.Revision.SHA)
	}
}

// TestBundleKeepsParentRelativeIncludes proves a Bundle accepts the same
// "../tasks/x.sh" layout DirFS does — the shared-directory-next-to-pipelines
// layout include.go's readFile documents as legitimate, not a hole to close.
func TestBundleKeepsParentRelativeIncludes(t *testing.T) {
	t.Parallel()

	bundle := Bundle{
		"pipelines/app.yml": `
tasks:
- name: unit
  run_file: ../tasks/unit.sh
jobs:
- name: build
  plan:
  - task: unit
`,
		"../tasks/unit.sh": "echo shared\n",
	}

	cfg, err := LoadFS(bundle, "pipelines/app.yml", "app", nil)
	if err != nil {
		t.Fatalf("LoadFS(Bundle): %v", err)
	}

	if got, want := cfg.Tasks[0].Run, "echo shared\n"; got != want {
		t.Errorf("Tasks[0].Run = %q, want %q", got, want)
	}
}

// TestBundleNeverReadsTheDaemonsFilesystem proves a Bundle refuses an include
// it was not sent rather than falling back to disk — the closed-set
// guarantee config.Bundle's doc comment promises.
func TestBundleNeverReadsTheDaemonsFilesystem(t *testing.T) {
	t.Parallel()

	bundle := Bundle{}

	_, err := bundle.ReadFile("ci/unit.sh")
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadFile of an unbundled path: got %v, want an os.ErrNotExist-wrapping error", err)
	}
}

// TestBuiltinPromptsStillResolveFromTheBundlePath proves an @builtin/ include
// resolves through the embedded prompt library even when everything else
// loads from a Bundle rather than disk — it never touches r.files.
func TestBuiltinPromptsStillResolveFromTheBundlePath(t *testing.T) {
	t.Parallel()

	bundle := Bundle{
		"pipeline.yml": `
agents:
- name: reviewer
  system_file: "@builtin/builder"
  source:
    model: openai/gpt-4o
jobs:
- name: build
  plan:
  - agent: reviewer
`,
	}

	cfg, err := LoadFS(bundle, "pipeline.yml", "app", nil)
	if err != nil {
		t.Fatalf("LoadFS(Bundle): %v", err)
	}

	if cfg.Agents[0].System == "" {
		t.Errorf("Agents[0].System is empty, want the builtin prompt's content")
	}
}

// TestLoadFSErrorsNameThePathOnDisk holds the local error messages to the
// path the user typed. Loading through an fs.FS-shaped seam makes it easy to
// report the bare base name instead, which turns "pipeline YAML
// "ci/app.yml": ..." into "pipeline YAML "app.yml": ..." — useless when
// several pipelines share a base name, which is the exact situation --name
// exists for.
func TestLoadFSErrorsNameThePathOnDisk(t *testing.T) {
	t.Parallel()

	dirPath := writeConfig(t, `
tasks:
- name: unit
  run_file: ci/missing.sh
jobs:
- name: build
  plan:
  - task: unit
`)

	_, err := LoadConfig(dirPath)
	if err == nil {
		t.Fatal("LoadConfig: expected an error for a missing run_file")
	}

	dir := filepath.Dir(dirPath)

	if !strings.Contains(err.Error(), dirPath) {
		t.Errorf("error %q does not name the pipeline file %q", err, dirPath)
	}

	if !strings.Contains(err.Error(), dir) {
		t.Errorf("error %q does not name the directory the include was looked for in (%q)", err, dir)
	}
}

// TestBundleLoadErrorsCiteTheUploadNotADirectory is the other half: a daemon
// has no directory the sender would recognise, so it must not invent one.
func TestBundleLoadErrorsCiteTheUploadNotADirectory(t *testing.T) {
	t.Parallel()

	bundle := Bundle{"pipeline.yml": `
tasks:
- name: unit
  run_file: ci/missing.sh
jobs:
- name: build
  plan:
  - task: unit
`}

	_, err := LoadFS(bundle, "pipeline.yml", "app", nil)
	if err == nil {
		t.Fatal("LoadFS(Bundle): expected an error for an unbundled run_file")
	}

	if !strings.Contains(err.Error(), "uploaded pipeline configuration") {
		t.Errorf("error %q does not say the include was missing from the upload", err)
	}
}

// flakyFiles serves each path once and fails every read after that, standing
// in for a file deleted between the resolver reading it and the revision
// re-reading it to hash it.
type flakyFiles struct {
	files Files
	seen  map[string]bool
}

func (f *flakyFiles) ReadFile(name string) ([]byte, error) {
	if f.seen[name] {
		return nil, fmt.Errorf("%s: %w", name, os.ErrNotExist)
	}

	f.seen[name] = true

	data, err := f.files.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	return data, nil
}

// TestIncludeVanishingMidLoadFailsTheLoad covers the load's one real
// time-of-check race: an include is read twice, once to inline it and once to
// hash it, and a file that disappears in between must fail the load rather
// than produce a revision hashed over bytes nothing ran.
func TestIncludeVanishingMidLoadFailsTheLoad(t *testing.T) {
	t.Parallel()

	fsys := &flakyFiles{
		seen: map[string]bool{},
		files: Bundle{
			"pipeline.yml": `
tasks:
- name: unit
  run_file: ci/unit.sh
jobs:
- name: build
  plan:
  - task: unit
`,
			"ci/unit.sh": "echo hi\n",
		},
	}

	_, err := LoadFS(fsys, "pipeline.yml", "app", nil)
	if err == nil {
		t.Fatal("LoadFS: expected an error when an include vanishes mid-load")
	}

	if !strings.Contains(err.Error(), "ci/unit.sh") {
		t.Errorf("error %q does not name the include that vanished", err)
	}
}

func TestValidPipelineNameRefusesUnsafeIdentities(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		ok   bool
	}{
		{"app", true},
		{"web-app_2", true},
		{"app.staging", true},
		{"", false},
		{".", false},
		{"..", false},
		{"a/b", false},
		{"a\\b", false},
		{"has space", false},
		{"has\ttab", false},
		// The name is concatenated into URLs, never escaped into them: each
		// of these truncates or re-decodes the path segment it lands in.
		{"app?x=1", false},
		{"app#frag", false},
		{"app%2e%2e", false},
		{"app:8088", false},
		{"<script>", false},
		{strings.Repeat("a", maxPipelineNameLength+1), false},
	}

	for _, tc := range cases {
		err := ValidPipelineName(tc.name)
		if tc.ok && err != nil {
			t.Errorf("ValidPipelineName(%q): unexpected error: %v", tc.name, err)
		}

		if !tc.ok && err == nil {
			t.Errorf("ValidPipelineName(%q): expected an error, got nil", tc.name)
		}
	}
}
