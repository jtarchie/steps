package config

import (
	"errors"
	"strings"
	"testing"
)

// TestFormatMovesAPipelineAssertLastWithoutDoublingABlankLine: the one top-level key that moves can leave a blank line the encoder already wrote under the file's head comment, and two would be a run.
func TestFormatMovesAPipelineAssertLastWithoutDoublingABlankLine(t *testing.T) {
	t.Parallel()

	const source = "# head\n\nassert:\n  execution: [a]\n\njobs:\n- name: a\n  plan:\n  - task: t\n    run: \"true\"\n"
	const want = "# head\n\njobs:\n  - name: a\n    plan:\n      - task: t\n        run: \"true\"\n\nassert:\n  execution: [a]\n"

	got, err := Format([]byte(source), Bundle{})
	if err != nil {
		t.Fatal(err)
	}

	if string(got) != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// TestFormatKeepsAnEmojiBlockABlock: yaml.v3 (and yaml.v4 by default) calls anything above U+FFFF unprintable, so a `run: |` holding 🤖 came out as one double-quoted string, and a `#` line further down read as a lost comment and fmt refused the file (pipelines' work/linear-release.yml). WithSupplementaryUnicode is the fix.
func TestFormatKeepsAnEmojiBlockABlock(t *testing.T) {
	t.Parallel()

	const source = "jobs:\n- name: a\n  plan:\n  - task: t\n    run: |\n      printf '🤖 done\\n'\n      # not a YAML comment\n      true\n"
	const want = "jobs:\n  - name: a\n    plan:\n      - task: t\n        run: |\n          printf '🤖 done\\n'\n          # not a YAML comment\n          true\n"

	got, err := Format([]byte(source), Bundle{})
	if err != nil {
		t.Fatal(err)
	}

	if string(got) != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// TestFormatNeverWrapsALongLine: yaml.v4's v3 preset wraps at 80 columns where yaml.v3's encoder never did, which would fold every long description: and flow mapping onto a second line.
func TestFormatNeverWrapsALongLine(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("word ", 30)
	source := "jobs:\n- name: a\n  plan:\n  - task: t\n    run: \"echo " + long + "\"\n"
	want := "jobs:\n  - name: a\n    plan:\n      - task: t\n        run: \"echo " + long + "\"\n"

	got, err := Format([]byte(source), Bundle{})
	if err != nil {
		t.Fatal(err)
	}

	if string(got) != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// TestFormatRefusesWhatIsNotAPipeline:a vars file, a task file and a multi-document file are not pipelines, so a directory walk passes over them.
func TestFormatRefusesWhatIsNotAPipeline(t *testing.T) {
	t.Parallel()

	for _, source := range []string{
		"repo: acme/app\n",
		"platform: linux\nrun: {path: make}\njobs: []\n",
		"jobs: []\n---\njobs: []\n",
		"- jobs\n",
		"",
	} {
		_, err := Format([]byte(source), Bundle{})
		if !errors.Is(err, ErrNotPipeline) {
			t.Errorf("Format(%q) = %v, want ErrNotPipeline", source, err)
		}
	}
}

// TestSameMeaningRefuses is the guard on its own: the formatter never produces these, so only a direct call can show it would refuse them.
func TestSameMeaningRefuses(t *testing.T) {
	t.Parallel()

	const before = "jobs:\n- name: a # why\n  plan:\n  - task: t\n    run: \"true\"\n"

	for _, tc := range []struct{ name, after, want string }{
		{"a changed value", strings.Replace(before, `"true"`, `"false"`, 1), "decode to different values"},
		{"a value changed type", strings.Replace(before, `"true"`, "true", 1), "decode to different values"},
		{"a dropped comment", strings.Replace(before, " # why", "", 1), `lose the comment "# why"`},
		{"a reworded comment", strings.Replace(before, "# why", "# because", 1), `lose the comment "# why"`},
		{"a result that does not parse", before + "  - [\n", "would not decode"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := sameMeaning([]byte(before), []byte(tc.after), Bundle{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}

	const relaid = "jobs:\n  - name: a    # why\n    plan:\n      - task: t\n        run: \"true\"\n"

	err := sameMeaning([]byte(before), []byte(relaid), Bundle{})
	if err != nil {
		t.Errorf("a layout-only change was refused: %v", err)
	}
}
