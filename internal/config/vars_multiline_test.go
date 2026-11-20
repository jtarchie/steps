package config

import (
	"strings"
	"testing"
)

// TestMultiLineVarsLandOnlyWhereTextCanCarryThem pins each scalar style: a | block takes a multi-line value as the lines Concourse's structural substitution would give, every other context is refused by var and line, and a value with no newline substitutes byte for byte as it always did.
func TestMultiLineVarsLandOnlyWhereTextCanCarryThem(t *testing.T) {
	t.Parallel()

	vars := map[string]string{"note": "first\n\n  second\n", "one": "single", "tail": "folded\n"}

	for name, c := range map[string]struct {
		source string
		want   string
		err    string
	}{
		"a single-line value substitutes byte for byte in any style": {
			source: "a: ((one))\nb: \"((one))\"\nc: >\n  x ((one))\n((one)): k # ((one))\n",
			want:   "a: single\nb: \"single\"\nc: >\n  x single\nsingle: k # single\n",
		},
		"a trailing newline alone does not make a value multi-line": {
			source: "a: echo ((tail)) more\n",
			want:   "a: echo folded more\n",
		},
		"a literal block indents continuation lines to its content and drops the trailing newline": {
			source: "a:\n  run: |\n    echo ((note)) end\n    done\nb: 1\n",
			want:   "a:\n  run: |\n    echo first\n\n      second end\n    done\nb: 1\n",
		},
		"strip and keep chomping are literal blocks too": {
			source: "a: |-\n  ((note))\nb: |+\n  ((note))\n\n",
			want:   "a: |-\n  first\n\n    second\nb: |+\n  first\n\n    second\n\n",
		},
		"a blank line inside the block does not end it": {
			source: "a: |\n  x\n\n  ((note))\n",
			want:   "a: |\n  x\n\n  first\n\n    second\n",
		},
		"a more-indented line still continues at the block's own indentation": {
			source: "a: |\n  if x; then\n      ((note))\n  fi\n",
			want:   "a: |\n  if x; then\n      first\n\n    second\n  fi\n",
		},
		"an explicit indentation indicator sets the block's indentation": {
			source: "a: |4\n      lead\n    ((note))\n",
			want:   "a: |4\n      lead\n    first\n\n      second\n",
		},
		"a plain scalar is refused": {
			source: "a: 1\nb: echo ((note))\n",
			err:    `p.yml:2: var "note" has a multi-line value, used in a plain value`,
		},
		"a single-quoted string is refused": {
			source: "b: 'echo ((note))'\n",
			err:    `p.yml:1: var "note" has a multi-line value, used in a quoted string`,
		},
		"a double-quoted string is refused": {
			source: "b: [\"((note))\"]\n",
			err:    `p.yml:1: var "note" has a multi-line value, used in a quoted string`,
		},
		"a folded block is refused": {
			source: "b: >\n  x\n  ((note))\n",
			err:    `p.yml:3: var "note" has a multi-line value, used in a folded > block`,
		},
		"a mapping key is refused": {
			source: "b: 1\n((note)): x\n",
			err:    `p.yml:2: var "note" has a multi-line value, used in a mapping key`,
		},
		"a comment after a literal block has ended is refused": {
			source: "a: |\n  text\n# ((note))\n",
			err:    `p.yml:3: var "note" has a multi-line value, used outside any value`,
		},
		"a literal block's header line is refused": {
			source: "a: | # ((note))\n  text\n",
			err:    `p.yml:1: var "note" has a multi-line value, used outside any value`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := interpolateSource([]byte(c.source), "p.yml", vars)
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("err = %v, want it to contain %q", err, c.err)
				}

				return
			}

			if err != nil {
				t.Fatalf("interpolate: %v", err)
			}

			if string(got) != c.want {
				t.Errorf("got\n%q\nwant\n%q", got, c.want)
			}
		})
	}
}

// TestMultiLineVarInUnparsableSourceFallsBackToText: a source that does not parse before substitution has no structure to judge a var by, so it is substituted as text and left to the parse after.
func TestMultiLineVarInUnparsableSourceFallsBackToText(t *testing.T) {
	t.Parallel()

	got, err := interpolateSource([]byte("a: [((note))\n"), "p.yml", map[string]string{"note": "x\ny"})
	if err != nil {
		t.Fatalf("interpolate: %v", err)
	}

	if string(got) != "a: [x\ny\n" {
		t.Errorf("got %q", got)
	}
}
