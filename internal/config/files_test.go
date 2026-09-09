package config

// The two halves a `steps pipeline set` rests on: a name that is safe to be a
// URL segment and an identity, and a bundle that resolves an upload's includes
// without reaching for the daemon's own disk.

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// TestValidPipelineNameRefusesWhatBreaksAURL is the guarantee that makes the
// identity safe to be a NAME rather than a path.
//
// The name is CONCATENATED into "/p/"+slug rather than escaped into it, and it
// is a database identity besides — so the characters that break either are
// refused once, here, rather than escaped at each of the dozen places that use
// one. A path could never traverse or truncate a route; a name can.
func TestValidPipelineNameRefusesWhatBreaksAURL(t *testing.T) {
	t.Parallel()

	for _, name := range []string{
		"", ".", "..", "../etc/passwd", "a/b", "a?b", "a#b", "a%2e", "a b", "a\nb",
		strings.Repeat("a", maxPipelineNameLength+1),
	} {
		if ValidPipelineName(name) == nil {
			t.Errorf("%q was accepted as a pipeline name", name)
		}
	}

	for _, name := range []string{"app", "infra-2", "my.pipeline", "A_b", "0"} {
		err := ValidPipelineName(name)
		if err != nil {
			t.Errorf("%q was refused: %v", name, err)
		}
	}
}

// TestBundleIsClosed is the security property of an upload: a daemon
// resolving an include it was not sent must FAIL rather than fall back to
// reading its own filesystem, or a crafted run_file: reads daemon-local files
// the sender never uploaded.
func TestBundleIsClosed(t *testing.T) {
	t.Parallel()

	bundle := Bundle{"ci/unit.sh": "echo hi\n"}

	_, err := bundle.ReadFile("/etc/passwd")
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a Bundle answered for an absolute path: %v", err)
	}

	_, err = bundle.ReadFile("../../etc/passwd")
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a Bundle answered for a traversal: %v", err)
	}

	// The one path it does answer for is the one it was given — cleaned,
	// because a sender keys its bundle off Revision.Includes and the resolver
	// may ask for the same file as ./ci/unit.sh.
	for _, spelling := range []string{"ci/unit.sh", "./ci/unit.sh"} {
		body, err := bundle.ReadFile(spelling)
		if err != nil || string(body) != "echo hi\n" {
			t.Errorf("Bundle.ReadFile(%q) = (%q, %v)", spelling, body, err)
		}
	}
}

// TestParseResolvesIncludesFromTheBundleAndNotFromDisk is the seam a daemon
// stands on: it has no sibling filesystem, so the same configuration parsed
// against an upload must produce the same hash a local load did — and must
// not read a file that happens to exist beside the process.
func TestParseResolvesIncludesFromTheBundleAndNotFromDisk(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
jobs:
- name: build
  plan:
  - task: compile
    inputs: []
    run_file: ci/unit.sh
`)
	writeSibling(t, path, "ci/unit.sh", "echo from-disk\n")

	local, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if local.Revision.Includes["ci/unit.sh"] != "echo from-disk\n" {
		t.Fatalf("a local load carries %q", local.Revision.Includes["ci/unit.sh"])
	}

	// The same bytes and the same includes: one configuration, one hash,
	// whichever side of an upload it was read on.
	uploaded, err := Parse([]byte(local.Revision.Source), Slugify(path), Bundle(local.Revision.Includes))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if uploaded.Revision.SHA != local.Revision.SHA {
		t.Errorf("the uploaded configuration hashes to %s, the local one to %s — a set could never be recognised as unchanged",
			uploaded.Revision.SHA, local.Revision.SHA)
	}

	// A bundle that does NOT carry the include is refused rather than served
	// from the daemon's own disk, even though the file is right there.
	_, err = Parse([]byte(local.Revision.Source), Slugify(path), Bundle{})
	if err == nil {
		t.Fatal("a configuration whose include was not uploaded parsed anyway")
	}

	if !strings.Contains(err.Error(), "uploaded pipeline configuration") {
		t.Errorf("the refusal does not say where it looked: %v", err)
	}
}

// TestRevisionCoversTheIncludesContent: a run_file: decides what a step
// executes, so editing one is editing the pipeline — and a set of the same
// YAML over a changed script has to be a different configuration.
func TestRevisionCoversTheIncludesContent(t *testing.T) {
	t.Parallel()

	source := "jobs:\n- name: build\n  plan:\n  - task: compile\n    inputs: []\n    run_file: ci/unit.sh\n"

	first, err := Parse([]byte(source), "app", Bundle{"ci/unit.sh": "echo one\n"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	second, err := Parse([]byte(source), "app", Bundle{"ci/unit.sh": "echo two\n"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if first.Revision.SHA == second.Revision.SHA {
		t.Error("one YAML over two scripts is one configuration, so a set that changed the script would read as unchanged")
	}
}
