package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSplitPositional(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		args     []string
		wantPath string
		wantRest []string
	}{
		{"positional first", []string{"pipeline.yml", "--job", "x"}, "pipeline.yml", []string{"--job", "x"}},
		{"positional only", []string{"pipeline.yml"}, "pipeline.yml", nil},
		{"only flags", []string{"--job", "x"}, "", []string{"--job", "x"}},
		{"empty", nil, "", nil},
		// A value-taking flag's value must not be mistaken for the path, so
		// the path resolves the same whether it comes before or after flags.
		{"path after flag and its value", []string{"--job", "x", "pipeline.yml"}, "pipeline.yml", []string{"--job", "x"}},
		{"path after attached flag value", []string{"--job=x", "pipeline.yml"}, "pipeline.yml", []string{"--job=x"}},
		{"version flag value skipped", []string{"--version", "n=1", "pipeline.yml"}, "pipeline.yml", []string{"--version", "n=1"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gotPath, gotRest := splitPositional(tt.args)
			if gotPath != tt.wantPath {
				t.Errorf("path = %q, want %q", gotPath, tt.wantPath)
			}

			if len(gotRest) != 0 || len(tt.wantRest) != 0 {
				if !reflect.DeepEqual(gotRest, tt.wantRest) {
					t.Errorf("rest = %v, want %v", gotRest, tt.wantRest)
				}
			}
		})
	}
}

func TestVersionFlagListSet(t *testing.T) {
	t.Parallel()

	t.Run("accumulates pairs", func(t *testing.T) {
		t.Parallel()

		list := make(versionFlagList)

		for _, arg := range []string{"number=87", "ref=abc"} {
			err := list.Set(arg)
			if err != nil {
				t.Fatalf("Set(%q): %v", arg, err)
			}
		}

		want := versionFlagList{"number": "87", "ref": "abc"}
		if !reflect.DeepEqual(list, want) {
			t.Errorf("list = %v, want %v", list, want)
		}
	})

	t.Run("rejects malformed", func(t *testing.T) {
		t.Parallel()

		for _, bad := range []string{"noequals", "=novalue"} {
			list := make(versionFlagList)

			err := list.Set(bad)
			if err == nil {
				t.Errorf("Set(%q) = nil, want error", bad)
			}
		}
	})
}

func TestVersionMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		version    any
		wantMode   string
		wantPinned map[string]string
	}{
		{"unset", nil, "latest", nil},
		{"latest string", "latest", "latest", nil},
		{"every string", "every", "every", nil},
		{"pinned any", map[string]any{"number": 87}, "pinned", map[string]string{"number": "87"}},
		{"pinned string", map[string]string{"ref": "abc"}, "pinned", map[string]string{"ref": "abc"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mode, pinned := VersionMode(Step{Version: tt.version})
			if mode != tt.wantMode {
				t.Errorf("mode = %q, want %q", mode, tt.wantMode)
			}

			if !reflect.DeepEqual(pinned, tt.wantPinned) {
				t.Errorf("pinned = %v, want %v", pinned, tt.wantPinned)
			}
		})
	}
}

func TestSelectVersion(t *testing.T) {
	t.Parallel()

	versions := []map[string]any{
		{"number": 1},
		{"number": 2},
		{"number": 3},
	}

	t.Run("latest when unpinned", func(t *testing.T) {
		t.Parallel()

		got, err := SelectVersion(versions, nil)
		if err != nil {
			t.Fatal(err)
		}

		if got["number"] != 3 {
			t.Errorf("got %v, want the last version", got)
		}
	})

	t.Run("matches pin", func(t *testing.T) {
		t.Parallel()

		got, err := SelectVersion(versions, map[string]string{"number": "2"})
		if err != nil {
			t.Fatal(err)
		}

		if got["number"] != 2 {
			t.Errorf("got %v, want number=2", got)
		}
	})

	t.Run("error on no versions", func(t *testing.T) {
		t.Parallel()

		_, err := SelectVersion(nil, nil)
		if err == nil {
			t.Error("expected error for empty versions")
		}
	})

	t.Run("error on unmatched pin", func(t *testing.T) {
		t.Parallel()

		_, err := SelectVersion(versions, map[string]string{"number": "99"})
		if err == nil {
			t.Error("expected error for unmatched pin")
		}
	})
}

func TestRender(t *testing.T) {
	t.Parallel()

	data := map[string]any{
		"source":  map[string]any{"repo": "jtarchie/ci"},
		"version": map[string]any{"number": 87},
	}

	t.Run("renders fields", func(t *testing.T) {
		t.Parallel()

		got, err := Render("clone {{ .source.repo }} at {{ .version.number }}", data)
		if err != nil {
			t.Fatal(err)
		}

		if want := "clone jtarchie/ci at 87"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("errors on missing key", func(t *testing.T) {
		t.Parallel()

		_, err := Render("{{ .source.nope }}", data)
		if err == nil {
			t.Error("expected error for missing key")
		}
	})

	t.Run("errors on malformed template", func(t *testing.T) {
		t.Parallel()

		_, err := Render("{{ .source.repo ", data)
		if err == nil {
			t.Error("expected error for malformed template")
		}
	})
}

func TestLoadConfigAndLookups(t *testing.T) {
	t.Parallel()

	const pipeline = `
resource_types:
- name: pull-request
  config:
    check: gh pr list
resources:
- name: prs
  type: pull-request
  source:
    repo: jtarchie/ci
jobs:
- name: review
  plan:
  - get: prs
  - task: review
    run: echo hi
`

	path := filepath.Join(t.TempDir(), "pipeline.yml")

	err := os.WriteFile(path, []byte(pipeline), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	if got := cfg.JobNames(); !reflect.DeepEqual(got, []string{"review"}) {
		t.Errorf("JobNames() = %v, want [review]", got)
	}

	_, err = cfg.FindResource("prs")
	if err != nil {
		t.Errorf("FindResource(prs): %v", err)
	}

	_, err = cfg.FindResourceType("pull-request")
	if err != nil {
		t.Errorf("FindResourceType(pull-request): %v", err)
	}

	_, err = cfg.FindJob("review")
	if err != nil {
		t.Errorf("FindJob(review): %v", err)
	}

	missingLookups := []func() error{
		func() error { _, err := cfg.FindResource("nope"); return err },
		func() error { _, err := cfg.FindResourceType("nope"); return err },
		func() error { _, err := cfg.FindJob("nope"); return err },
	}
	for _, lookup := range missingLookups {
		err := lookup()
		if err == nil {
			t.Error("expected error looking up a missing name")
		}
	}
}

func TestLoadConfigErrors(t *testing.T) {
	t.Parallel()

	t.Run("missing file", func(t *testing.T) {
		t.Parallel()

		_, err := LoadConfig(filepath.Join(t.TempDir(), "absent.yml"))
		if err == nil {
			t.Error("expected error for missing file")
		}
	})

	t.Run("invalid yaml", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "bad.yml")

		err := os.WriteFile(path, []byte("jobs: [oops"), 0o600)
		if err != nil {
			t.Fatal(err)
		}

		_, err = LoadConfig(path)
		if err == nil {
			t.Error("expected error for invalid YAML")
		}
	})
}
