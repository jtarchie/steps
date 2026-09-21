package agent

// The container half of the file tools, against real images: they run whenever a daemon is reachable and skip cleanly when one is not, because a test that only runs when somebody opts in is how a shipped feature stays broken without anybody noticing.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/shell"
)

// probeImages are the two userlands worth disagreeing: busybox, where grep has no -P and accepts -I without honouring it, and GNU, where -E reads `\d` as a literal d. Anything that behaves the same in both behaves the same nearly everywhere.
var probeImages = []string{"alpine:3", "debian:13-slim"}

func requireAgentDocker(t *testing.T) {
	t.Helper()

	_, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("docker not found on PATH")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err = exec.CommandContext(ctx, "docker", "info").Run()
	if err != nil {
		t.Skip("docker daemon not reachable (`docker info` failed)")
	}
}

// daemonVisibleDir is a directory the daemon can actually bind-mount. On macOS the daemon runs in a VM that shares the user's home and not $TMPDIR, and an unshared bind mount is answered with an EMPTY directory rather than an error — so a test using t.TempDir() would pass while proving nothing.
func daemonVisibleDir(t *testing.T) string {
	t.Helper()

	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory to root a daemon-visible workspace in")
	}

	dir, err := os.MkdirTemp(home, ".steps-agent-test-*")
	if err != nil {
		t.Skipf("cannot create a daemon-visible temp dir under %s: %v", home, err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	return dir
}

// newContainerTree starts a container over dir and returns the tree its file tools would use.
func newContainerTree(t *testing.T, image, dir string) containerTree {
	t.Helper()

	runner, err := shell.NewRunner(shell.RunnerSpec{Image: image, Cwd: dir})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	t.Cleanup(func() { shell.CloseRunner(runner, "test") })

	caps, err := probeUserland(t.Context(), runner, image)
	if err != nil {
		t.Fatalf("probeUserland(%s): %v", image, err)
	}

	return containerTree{runner: runner, dir: caps.pwd, lost: &lostTree{}}
}

// writeFixture lays down the tree both halves are asked about.
func writeFixture(t *testing.T, dir string) {
	t.Helper()

	files := map[string]string{
		"main.go":                "package main\n\nfunc main() { x := 42 }\nvar re = 1\n",
		"a file with spaces.txt": "alpha 7\nbeta\ngamma 99\n",
		"weird:name.txt":         "colon 7\n",
		"sub/deep.go":            "package sub\n\nfunc Deep() int { return 7 }\n",
		".git/objects.txt":       "pruned 7\n",
		"node_modules/pkg.js":    "pruned 7\n",
		"vendor/v.go":            "pruned 7\n",
		"lines.txt":              "line1\nline2\nline3\nline4\nline5\n",
		// The four characters a POSIX bracket expression can only position, in a file a pattern naming them has to find.
		"brackets.txt": "a]b^c-d 7\n",
		// A backslash is the fourth character a bracket expression can only position, and the one Go's own regexp cannot be asked about: RE2 reads it as an escape inside brackets where POSIX reads it as a literal, so the rendered class is well-formed to grep and unparseable to Go. Real greps are the only judge available.
		"backslash.txt": "a\\\\b 7\n",
	}

	for name, body := range files {
		full := filepath.Join(dir, name)

		err := os.MkdirAll(filepath.Dir(full), 0o750)
		if err != nil {
			t.Fatal(err)
		}

		err = os.WriteFile(full, []byte(body), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	err := os.WriteFile(filepath.Join(dir, "blob.bin"), []byte("bin\x00\x01\x02 7 data\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	// Over the size a content scan refuses to open, and under no size rule at all for a filename search — which is the divergence a `find -size` applied to every candidate introduces.
	err = os.WriteFile(filepath.Join(dir, "big.txt"), []byte(strings.Repeat("x", maxSearchFileBytes+1)), 0o600)
	if err != nil {
		t.Fatal(err)
	}
}

// TestContainerTreeMatchesHostTree is the guarantee the shell-out puts at risk. The same fixture, the same call, the same answer — from Go walking this filesystem and from find and grep walking the container's. Anything that differs here is a pipeline whose results depend on where its agent ran, which is the one thing placement must not change.
func TestContainerTreeMatchesHostTree(t *testing.T) {
	requireAgentDocker(t)

	dir := daemonVisibleDir(t)
	writeFixture(t, dir)

	host := hostTree{dir: dir}

	for _, image := range probeImages {
		t.Run(image, func(t *testing.T) {
			contained := newContainerTree(t, image, dir)

			for _, tc := range parityCases() {
				t.Run(tc.name, func(t *testing.T) {
					want, err := host.search(t.Context(), dir, tc.opts)
					if err != nil {
						t.Fatalf("host search: %v", err)
					}

					got, err := contained.search(t.Context(), contained.dir, tc.opts)
					if err != nil {
						t.Fatalf("container search: %v", err)
					}

					compareSearchResults(t, want, got)
				})
			}

			// A base that IS one of the prune names. searchWalk skips a skip directory only below the base, so asking about `vendor` directly is an ordinary search — while a find that prunes by name alone answers with nothing at all.
			t.Run("base is a pruned directory", func(t *testing.T) {
				opts := mustOpts(t7Pattern, "", "content")

				want, err := host.search(t.Context(), filepath.Join(dir, "vendor"), opts)
				if err != nil {
					t.Fatalf("host search: %v", err)
				}

				if want.total == 0 {
					t.Fatal("the fixture no longer puts a match inside vendor, so this proves nothing")
				}

				got, err := contained.search(t.Context(), contained.dir+"/vendor", opts)
				if err != nil {
					t.Fatalf("container search: %v", err)
				}

				compareSearchResults(t, want, got)
			})
		})
	}
}

type parityCase struct {
	name string
	opts searchOpts
}

// parityCases covers each thing the container does differently from a Go walk: find's prune list and size filter, the glob applied on this side, grep's own output parsed back, and a pattern whose meaning the two greps disagree about until it is rendered.
func parityCases() []parityCase {
	return []parityCase{
		{"literal files_with_matches", mustOpts(t7Pattern, "", "files_with_matches")},
		{"literal content", mustOpts(t7Pattern, "", "content")},
		{"literal count", mustOpts(t7Pattern, "", "count")},
		{"digit shorthand", mustOpts(`\d\d`, "", "content")},
		{"word shorthand", mustOpts(`func\s+\w+\(`, "", "content")},
		{"anchored", mustOpts(`^package `, "", "content")},
		{"glob only", mustOpts("", "**/*.go", "files_with_matches")},
		{"glob and pattern", mustOpts("7", "**/*.go", "content")},
		{"no matches", mustOpts("zzzznothing", "", "content")},
		// A class holding one of the four characters a bracket expression can only POSITION. Spelled as a collating symbol these compile on debian and are refused outright by musl, where grep exits before matching anything and the refusal reads as an empty tree.
		{"dash in a class", mustOpts(`[a-z-]+ 99`, "", "content")},
		{"bracket specials in a class", mustOpts(`[\]\^\-]`, "", "content")},
		{"backslash in a class", mustOpts(`[a\\b]+`, "", "content")},
		{"negated class", mustOpts(`[^ ]7`, "", "content")},
		{"word characters and a dash", mustOpts(`[a-zA-Z0-9_-]+`, "", "files_with_matches")},
		// A filename search applies no size rule on this machine, so it must apply none in the container either: big.txt has to be in both answers.
		{"glob only over a large file", mustOpts("", "**/*.txt", "files_with_matches")},
	}
}

const t7Pattern = "7"

func mustOpts(pattern, glob, mode string) searchOpts {
	opts, errResult := buildSearchOpts(map[string]any{}, pattern, glob, mode)
	if errResult != nil {
		panic(errResult["error"])
	}

	return opts
}

// compareSearchResults checks every field the model is shown, since a difference in any of them is a difference the pipeline author sees.
func compareSearchResults(t *testing.T, want, got searchResult) {
	t.Helper()

	if want.total != got.total {
		t.Errorf("total = %d, want %d", got.total, want.total)
	}

	if want.filesScanned != got.filesScanned {
		t.Errorf("files_scanned = %d, want %d", got.filesScanned, want.filesScanned)
	}

	compareStringSets(t, "files", want.files, got.files)

	if len(want.matches) != len(got.matches) {
		t.Fatalf("matches = %d, want %d\n got: %v\nwant: %v", len(got.matches), len(want.matches), got.matches, want.matches)
	}

	wantMatches := make(map[string]bool, len(want.matches))
	for _, m := range want.matches {
		wantMatches[matchKey(m)] = true
	}

	for _, m := range got.matches {
		if !wantMatches[matchKey(m)] {
			t.Errorf("match %s is not one the host found", matchKey(m))
		}
	}

	if len(want.counts) != len(got.counts) {
		t.Errorf("counts = %v, want %v", got.counts, want.counts)
	}
}

func matchKey(m searchMatch) string {
	return m.path + ":" + strconv.Itoa(m.line) + ":" + m.text
}

func compareStringSets(t *testing.T, label string, want, got []string) {
	t.Helper()

	if len(want) != len(got) {
		t.Errorf("%s = %v, want %v", label, got, want)

		return
	}

	seen := make(map[string]bool, len(want))
	for _, w := range want {
		seen[w] = true
	}

	for _, g := range got {
		if !seen[g] {
			t.Errorf("%s has %q, which the host did not report", label, g)
		}
	}
}

// TestContainerTreeConfinesPaths is the security half. A container's root holds a real /etc/passwd, so a symlink planted in the tree resolves to something — and the answer has to be the same refusal the host gives, not a file.
func TestContainerTreeConfinesPaths(t *testing.T) {
	requireAgentDocker(t)

	dir := daemonVisibleDir(t)
	writeFixture(t, dir)

	err := os.Symlink("/etc/passwd", filepath.Join(dir, "escape"))
	if err != nil {
		t.Fatal(err)
	}

	contained := newContainerTree(t, "alpine:3", dir)

	// The last two escape LEXICALLY and resolve to nothing: readlink cannot object to a path whose intermediate directories do not exist, so they are the only cases the string check alone refuses. Without them the symlink check answers every one of these, and a broken lexical check goes unnoticed — which is how it survived mutation.
	for _, rel := range []string{"escape", "../outside.txt", "/etc/passwd", "sub/../../elsewhere", "../nosuchdir/deeper.txt", "sub/../../nosuchdir/x/y.txt"} {
		t.Run(rel, func(t *testing.T) {
			_, err := contained.resolve(t.Context(), rel)
			if err == nil {
				t.Fatalf("resolve(%q) was allowed; it must be refused the way the host refuses it", rel)
			}

			if !strings.Contains(err.Error(), "escapes the working directory") {
				t.Errorf("resolve(%q) error = %v, want it to name the confinement", rel, err)
			}
		})
	}

	// A path inside the tree still resolves, or the refusal above would be proving nothing.
	_, err = contained.resolve(t.Context(), "sub/deep.go")
	if err != nil {
		t.Errorf("resolve of an ordinary path failed: %v", err)
	}
}

// TestContainerTreeRoundTripsFiles covers the write half in both directions, including the mode an edit must not strip: a rename would have replaced the inode and quietly dropped the executable bit off a checked-in script.
func TestContainerTreeRoundTripsFiles(t *testing.T) {
	requireAgentDocker(t)

	dir := daemonVisibleDir(t)
	writeFixture(t, dir)

	script := filepath.Join(dir, "run.sh")

	err := os.WriteFile(script, []byte("#!/bin/sh\necho one\n"), 0o755) //nolint:gosec // the executable bit is what this test is about
	if err != nil {
		t.Fatal(err)
	}

	contained := newContainerTree(t, "alpine:3", dir)

	err = contained.writeFile(t.Context(), contained.dir+"/run.sh", []byte("#!/bin/sh\necho two\n"), false)
	if err != nil {
		t.Fatalf("writeFile: %v", err)
	}

	info, err := os.Stat(script)
	if err != nil {
		t.Fatal(err)
	}

	if info.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v, want 0755 kept — the write replaced the file instead of writing through it", info.Mode().Perm())
	}

	body, err := os.ReadFile(script) //nolint:gosec // a path this test made
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(body), "echo two") {
		t.Errorf("content = %q, want the container's write", body)
	}

	// A quote, a newline and a percent sign all have to survive being carried in as one shell word.
	awkward := "it's \"quoted\"\nsecond 100%\n"

	err = contained.writeFile(t.Context(), contained.dir+"/awkward.txt", []byte(awkward), false)
	if err != nil {
		t.Fatalf("writeFile(awkward): %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "awkward.txt")) //nolint:gosec // a path this test made
	if err != nil {
		t.Fatal(err)
	}

	if string(got) != awkward {
		t.Errorf("content = %q, want %q", got, awkward)
	}
}

// TestContainerWriteRefusesNUL covers the byte no argv can carry. The content crosses into the container as one shell word, and a NUL in it is not an error the shell reports — it is where the shell stops reading, so the file would be written silently truncated. A NUL at the very start is the case a length check gets wrong.
func TestContainerWriteRefusesNUL(t *testing.T) {
	requireAgentDocker(t)

	dir := daemonVisibleDir(t)
	contained := newContainerTree(t, "alpine:3", dir)

	for name, content := range map[string][]byte{
		"leading":  []byte("\x00after the nul\n"),
		"embedded": []byte("before\x00after\n"),
	} {
		t.Run(name, func(t *testing.T) {
			err := contained.writeFile(t.Context(), contained.dir+"/nul-"+name+".txt", content, false)
			if !errors.Is(err, errNULInContent) {
				t.Fatalf("writeFile with a %s NUL = %v, want it refused before the shell truncates it", name, err)
			}

			_, statErr := os.Stat(filepath.Join(dir, "nul-"+name+".txt"))
			if statErr == nil {
				t.Error("the file was created anyway, so a truncated write reached the tree")
			}
		})
	}
}

// TestProbeRefusesAnImageWithoutTheFileUtilities is why the probe runs at preparation. An image too thin to answer a read_file has to say so before a token is spent, naming itself and what it lacks, rather than failing the model's first call with whatever the shell said.
func TestProbeRefusesAnImageWithoutTheFileUtilities(t *testing.T) {
	requireAgentDocker(t)

	dir := daemonVisibleDir(t)

	runner, err := shell.NewRunner(shell.RunnerSpec{Image: "alpine:3", Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { shell.CloseRunner(runner, "test") })

	// The image is fine; hiding the binaries is what makes it thin, and it proves the probe reads the container rather than assuming.
	_, _, _, err = runner.RunCaptureFullLimited(t.Context(), "rm -f /bin/find /usr/bin/find", 4096, "")
	if err != nil {
		t.Fatalf("preparing the fixture: %v", err)
	}

	_, err = probeUserland(t.Context(), runner, "alpine:3")
	if err == nil {
		t.Fatal("probeUserland accepted an image with no find")
	}

	for _, want := range []string{"alpine:3", "find"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to name %q", err, want)
		}
	}
}
