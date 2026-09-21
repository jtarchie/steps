package agent

// The parts of the container path that can be checked without a container.

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestShQuoteSurvivesTheShell is what stands in for an argv exec. Model-authored text reaches the container as one shell word, so anything this gets wrong is a path, a pattern or a file's contents being read as syntax — and `sh -c` is the only judge of that worth asking.
func TestShQuoteSurvivesTheShell(t *testing.T) {
	t.Parallel()

	_, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh to check quoting against")
	}

	cases := []string{
		"plain",
		"it's",
		`"double"`,
		"a b\tc",
		"semi; rm -rf /",
		"$(touch pwned)",
		"`touch pwned`",
		"back\\slash",
		"new\nline",
		"100%",
		"*glob*",
		"$HOME",
		"ünïcödé",
		`'\''nested'\''`,
		"",
	}

	for _, want := range cases {
		t.Run(want, func(t *testing.T) {
			t.Parallel()

			out, err := exec.CommandContext(t.Context(), "sh", "-c", "printf '%s' "+shQuote(want)).Output() //nolint:gosec // the tainted input IS the subject: shQuote has to make it inert
			if err != nil {
				t.Fatalf("sh rejected %s: %v", shQuote(want), err)
			}

			if string(out) != want {
				t.Errorf("sh read %s as %q, want %q", shQuote(want), out, want)
			}
		})
	}
}

// TestChunkQuotedBoundsTheQuotedLength is the property the chunking exists for. Splitting on raw bytes would still hand the kernel an argument list several times the size the split was chosen for, because every single quote quadruples on the way in — so a file full of them is exactly the case that would blow the limit.
func TestChunkQuotedBoundsTheQuotedLength(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"empty":       "",
		"short":       "hello",
		"all quotes":  strings.Repeat("'", 40_000),
		"mixed":       strings.Repeat("a'b", 30_000),
		"plain large": strings.Repeat("x", 100_000),
	}

	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var rebuilt strings.Builder

			for _, chunk := range chunkQuoted(content) {
				if len(chunk) > maxWriteChunkBytes*4 {
					t.Errorf("a quoted chunk is %d bytes, past what the bound allows for", len(chunk))
				}

				rebuilt.WriteString(unquote(t, chunk))
			}

			if rebuilt.String() != content {
				t.Errorf("chunks do not reassemble into the original (%d bytes vs %d)", rebuilt.Len(), len(content))
			}
		})
	}
}

// unquote reverses shQuote, so the test checks the chunks against what the shell would actually see rather than against how they were spelled.
func unquote(t *testing.T, quoted string) string {
	t.Helper()

	if !strings.HasPrefix(quoted, "'") || !strings.HasSuffix(quoted, "'") {
		t.Fatalf("chunk %q is not single-quoted", quoted)
	}

	return strings.ReplaceAll(quoted[1:len(quoted)-1], `'\''`, "'")
}

// TestSplitGrepPathPrefersTheLongestMatch covers the ambiguity grep's own output has and cannot resolve: it writes `path:line:text`, and a filename may contain a colon. Cutting at the first one attributes a file's matches to a file that does not exist.
func TestSplitGrepPathPrefersTheLongestMatch(t *testing.T) {
	t.Parallel()

	byPath := map[string]string{
		"/w/a.txt":        "a.txt",
		"/w/a.txt:1.txt":  "a.txt:1.txt",
		"/w/weird:name.g": "weird:name.g",
	}

	cases := []struct {
		row      string
		wantRel  string
		wantRest string
	}{
		{"/w/a.txt:7:hello", "a.txt", "7:hello"},
		{"/w/a.txt:1.txt:3:other", "a.txt:1.txt", "3:other"},
		{"/w/weird:name.g:1:colon 7", "weird:name.g", "1:colon 7"},
	}

	for _, tc := range cases {
		t.Run(tc.row, func(t *testing.T) {
			t.Parallel()

			rel, rest, ok := splitGrepPath(tc.row, byPath)
			if !ok {
				t.Fatalf("splitGrepPath(%q) found no file", tc.row)
			}

			if rel != tc.wantRel || rest != tc.wantRest {
				t.Errorf("splitGrepPath(%q) = %q, %q; want %q, %q", tc.row, rel, rest, tc.wantRel, tc.wantRest)
			}
		})
	}
}

// TestParseUserlandReadsTheProbe pins the probe's own format, which is the only thing standing between a thin image and a conversation that fails on its first tool call.
func TestParseUserlandReadsTheProbe(t *testing.T) {
	t.Parallel()

	got := parseUserland("pwd=/work\nmissing=find\nmissing=tr\n")

	if got.pwd != "/work" {
		t.Errorf("pwd = %q, want /work", got.pwd)
	}

	if strings.Join(got.missing, ",") != "find,tr" {
		t.Errorf("missing = %v, want [find tr] sorted", got.missing)
	}
}

// TestParseListRowsKeepsNamesWithSpaces is why list_dir asks find rather than parsing ls: a name with a space in it is ordinary, and a format that loses it loses the entry.
func TestParseListRowsKeepsNamesWithSpaces(t *testing.T) {
	t.Parallel()

	rows := "d\t0\t/w/sub\nf\t22\t/w/a file with spaces.txt\nf\t8\t/w/weird:name.txt\n"

	entries := parseListRows(rows, "/w")
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3: %v", len(entries), entries)
	}

	want := map[string]treeEntry{
		"sub":                    {name: "sub", isDir: true, size: 0},
		"a file with spaces.txt": {name: "a file with spaces.txt", size: 22},
		"weird:name.txt":         {name: "weird:name.txt", size: 8},
	}

	for _, got := range entries {
		expected, ok := want[got.name]
		if !ok {
			t.Errorf("unexpected entry %q", got.name)

			continue
		}

		if got != expected {
			t.Errorf("entry %q = %+v, want %+v", got.name, got, expected)
		}
	}
}

// TestSortLikeWalkDirMatchesTheHostWalk is what head_limit makes load-bearing: both sides cap the result, so two walks that visit the same files in a different order show the model a different subset. find lists a directory in whatever order it reads it, and filepath.WalkDir sorts each directory's entries — comparing whole paths is not the same thing, because '.' sorts below '/'.
func TestSortLikeWalkDirMatchesTheHostWalk(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	names := []string{"sub/deep.go", "sub.txt", "a.txt", "sub/a/b.txt", "zz.txt", "sub/A.txt", "sub-x/y.txt"}
	for _, name := range names {
		full := filepath.Join(dir, name)

		err := os.MkdirAll(filepath.Dir(full), 0o750)
		if err != nil {
			t.Fatal(err)
		}

		err = os.WriteFile(full, []byte("x\n"), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	var want []string

	walkErr := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if !d.IsDir() {
			want = append(want, p)
		}

		return nil
	})
	if walkErr != nil {
		t.Fatalf("WalkDir: %v", walkErr)
	}

	// Reversed, so a function that did nothing at all cannot pass.
	got := slices.Clone(want)
	slices.Reverse(got)
	sortLikeWalkDir(got)

	if !slices.Equal(got, want) {
		t.Errorf("sortLikeWalkDir = %v, want the order filepath.WalkDir produced %v", got, want)
	}
}
