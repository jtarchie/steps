package e2e

// Reading back a configuration a run executed.
//
// The revision each run pinned has been recorded since runs carried a
// revision_id, and nothing could show it: a run page said what happened and
// never what it was told to do, and the file on disk is only ever its newest
// version. These drive the pages that answer it.

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

// TestRunPageNamesTheConfigItRan is the headline: every run page says which
// configuration the run executed, and links the configuration itself.
func TestRunPageNamesTheConfigItRan(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")
	log := filepath.Join(dir, "build.log")

	revisionPipeline(t, path, "echo one >> "+log)
	mustRun(t, "run", path, "--job", "build")

	server, target := webServerFor(t, path)

	runs, err := target.Store.ListRuns(t.Context(), "build", 10)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}

	if len(runs) != 1 {
		t.Fatalf("got %d runs, want 1", len(runs))
	}

	sha := runs[0].ConfigSHA
	if sha == "" {
		t.Fatal("the run recorded no configuration")
	}

	code, page := webGet(t, server, "/p/"+target.Slug+"/runs/"+runs[0].ID)
	if code != http.StatusOK {
		t.Fatalf("run page = %d", code)
	}

	if !strings.Contains(page, sha[:12]) {
		t.Errorf("the run page does not name the configuration it ran:\n%s", page)
	}

	if !strings.Contains(page, "/config/"+sha) {
		t.Error("the run page does not link the configuration it ran")
	}

	// And the link resolves to the configuration ITSELF, which is the thing
	// git has and this daemon otherwise does not.
	code, config := webGet(t, server, "/p/"+target.Slug+"/config/"+sha)
	if code != http.StatusOK {
		t.Fatalf("config page = %d: %s", code, config)
	}

	if !strings.Contains(config, "echo one") {
		t.Errorf("the config page does not show what the run was told to do:\n%s", config)
	}
}

// TestConfigPageServesTheOldRevisionAfterAnEdit is the point of storing the
// source rather than only its hash: the configuration a run executed is
// readable after the file on disk has moved on, which is exactly when anyone
// asks.
func TestConfigPageServesTheOldRevisionAfterAnEdit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")
	log := filepath.Join(dir, "build.log")

	revisionPipeline(t, path, "echo one >> "+log)
	mustRun(t, "run", path, "--job", "build")

	revisionPipeline(t, path, "echo two >> "+log)
	mustRun(t, "run", path, "--job", "build")

	server, target := webServerFor(t, path)

	runs, err := target.Store.ListRuns(t.Context(), "build", 10)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}

	if len(runs) != 2 {
		t.Fatalf("got %d runs, want 2", len(runs))
	}

	// Newest first: runs[1] is the one whose configuration the file on disk
	// no longer holds.
	code, old := webGet(t, server, "/p/"+target.Slug+"/config/"+runs[1].ConfigSHA)
	if code != http.StatusOK {
		t.Fatalf("old config page = %d: %s", code, old)
	}

	if !strings.Contains(old, "echo one") {
		t.Errorf("the superseded configuration is not readable:\n%s", old)
	}

	if strings.Contains(old, "echo two") {
		t.Error("the old configuration page is showing the new configuration")
	}
}

// TestRunPageSaysTheConfigChanged is the question a failed run page exists to
// answer. The step-content diff already names which steps moved; this is its
// config-level twin, and without it a reader cannot tell an edited pipeline
// from an unlucky one.
//
// A failed run, because that is when the page asks: the diff is computed for
// failures only, where "what is different this time" is the first question,
// and on a green run it is trivia that costs a query.
func TestRunPageSaysTheConfigChanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")

	revisionPipeline(t, path, "echo one")
	mustRun(t, "run", path, "--job", "build")

	revisionPipeline(t, path, "exit 1")

	err := cli.Run([]string{"run", path, "--job", "build"})
	if err == nil {
		t.Fatal("the edited pipeline was supposed to fail")
	}

	server, target := webServerFor(t, path)

	runs, err := target.Store.ListRuns(t.Context(), "build", 10)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}

	if len(runs) != 2 {
		t.Fatalf("got %d runs, want 2", len(runs))
	}

	code, page := webGet(t, server, "/p/"+target.Slug+"/runs/"+runs[0].ID)
	if code != http.StatusOK {
		t.Fatalf("run page = %d", code)
	}

	if !strings.Contains(page, "configuration changed") {
		t.Errorf("the run page does not say the configuration moved since the last passed run:\n%s", page)
	}

	// Both sides named, so the reader can go and read the other one.
	if !strings.Contains(page, "/config/"+runs[1].ConfigSHA) {
		t.Error("the run page does not link the configuration it is being compared against")
	}
}

// TestRunPageIsQuietWhenTheConfigDidNotChange: the line is news, and a page
// that says it after every failure says nothing at all. Here one
// configuration passes and then fails — a flake, a full disk, the world
// moving — which is exactly the case a config-change claim would misdiagnose.
func TestRunPageIsQuietWhenTheConfigDidNotChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")
	marker := filepath.Join(dir, "marker")

	// Succeeds once, fails ever after, without the file changing.
	// `test -e`, not `[ -e ]`: a run: value opening with a bracket is a
	// YAML flow sequence.
	revisionPipeline(t, path, "test -e "+marker+" && exit 1 || touch "+marker)

	mustRun(t, "run", path, "--job", "build")

	err := cli.Run([]string{"run", path, "--job", "build", "--force"})
	if err == nil {
		t.Fatal("the second run was supposed to fail")
	}

	server, target := webServerFor(t, path)

	runs, err := target.Store.ListRuns(t.Context(), "build", 10)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}

	if len(runs) != 2 {
		t.Fatalf("got %d runs, want 2", len(runs))
	}

	if runs[0].ConfigSHA != runs[1].ConfigSHA {
		t.Fatalf("the fixture changed the configuration between runs (%s, %s)", runs[0].ConfigSHA, runs[1].ConfigSHA)
	}

	code, page := webGet(t, server, "/p/"+target.Slug+"/runs/"+runs[0].ID)
	if code != http.StatusOK {
		t.Fatalf("run page = %d", code)
	}

	if strings.Contains(page, "configuration changed") {
		t.Errorf("two runs of one configuration are being reported as a change:\n%s", page)
	}
}

// TestConfigPageRefusesAnUnknownRevision: a sha this pipeline never ran is a
// 404, not an empty page that reads as a configuration with nothing in it.
func TestConfigPageRefusesAnUnknownRevision(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")

	revisionPipeline(t, path, "echo one")
	mustRun(t, "run", path, "--job", "build")

	server, target := webServerFor(t, path)

	code, _ := webGet(t, server, "/p/"+target.Slug+"/config/"+strings.Repeat("a", 64))
	if code != http.StatusNotFound {
		t.Errorf("an unknown revision = %d, want 404", code)
	}
}
