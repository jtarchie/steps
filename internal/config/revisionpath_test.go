package config

// The revision hash folds the include paths in, so what those strings ARE
// decides whether the same pipeline is one configuration or two.

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRevisionIsTheSameHoweverThePipelineIsNamed: `steps run ci/app.yml` and
// `steps run /repo/ci/app.yml` are the same file, and a CONFIG column that
// moved between them would report an edit nobody made — on the one question
// the column exists to answer.
//
// No t.Parallel: t.Chdir is what makes the relative spelling reachable.
func TestRevisionIsTheSameHoweverThePipelineIsNamed(t *testing.T) {
	dir := t.TempDir()

	err := os.WriteFile(filepath.Join(dir, "build.sh"), []byte("echo building\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	pipeline := "jobs:\n- name: build\n  plan:\n  - task: compile\n    inputs: []\n    run_file: build.sh\n"

	err = os.WriteFile(filepath.Join(dir, "app.yml"), []byte(pipeline), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	absolute, err := Load(filepath.Join(dir, "app.yml"), "app", nil)
	if err != nil {
		t.Fatalf("Load(absolute): %v", err)
	}

	t.Chdir(dir)

	relative, err := Load("app.yml", "app", nil)
	if err != nil {
		t.Fatalf("Load(relative): %v", err)
	}

	if absolute.Revision.SHA != relative.Revision.SHA {
		t.Errorf("the same pipeline hashes differently by how it was named: %s vs %s",
			absolute.Revision.SHA, relative.Revision.SHA)
	}
}
