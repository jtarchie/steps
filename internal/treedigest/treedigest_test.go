package treedigest_test

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"math/rand/v2"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jtarchie/steps/internal/treedigest"
)

func requireDocker(t *testing.T) {
	t.Helper()

	_, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("docker not found on PATH")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	err = exec.CommandContext(ctx, "docker", "info").Run()
	if err != nil {
		t.Skip("docker daemon not reachable (`docker info` failed)")
	}
}

func digest(t *testing.T, fsys fs.FS) string {
	t.Helper()

	sum, err := treedigest.TreeOf(fsys)
	if err != nil {
		t.Fatalf("TreeOf: %v", err)
	}

	return sum
}

func edgeTrees() map[string]fstest.MapFS {
	return map[string]fstest.MapFS{
		"empty":         {},
		"empty dir":     {"a": {Mode: fs.ModeDir | 0o755}},
		"empty file":    {"a": {Mode: 0o644}},
		"exec only":     {"a": {Data: []byte("#!/bin/sh\n"), Mode: 0o755}},
		"group exec":    {"a": {Data: []byte("x"), Mode: 0o610}},
		"newline name":  {"a\nb": {Data: []byte("x"), Mode: 0o644}, "a": {Data: []byte("y"), Mode: 0o755}},
		"newline exec":  {"d/x\n": {Data: []byte("x"), Mode: 0o755}},
		"trailing nl":   {"l": {Data: []byte("target\n"), Mode: fs.ModeSymlink}},
		"link":          {"l": {Data: []byte("../etc/passwd"), Mode: fs.ModeSymlink}},
		"dangling":      {"d/l": {Data: []byte("nowhere"), Mode: fs.ModeSymlink}},
		"fifo":          {"p": {Mode: fs.ModeNamedPipe | 0o644}},
		"glob chars":    {"*?[a]\\-'\" ": {Data: []byte("x"), Mode: 0o644}},
		"dash name":     {"-": {Data: []byte("x"), Mode: 0o644}, "--": {Mode: fs.ModeDir | 0o755}},
		"prefix order":  {"a": {Mode: fs.ModeDir | 0o755}, "a/b": {Data: []byte("x"), Mode: 0o644}, "a-b": {Data: []byte("y"), Mode: 0o644}, "a b": {Data: []byte("z"), Mode: 0o644}},
		"same content":  {"a": {Data: []byte("x"), Mode: 0o644}, "b": {Data: []byte("x"), Mode: 0o644}},
		"long name":     {strings.Repeat("n", 200) + "/" + strings.Repeat("m", 200): {Data: []byte("x"), Mode: 0o644}},
		"big file":      {"big": {Data: bytes.Repeat([]byte("0123456789"), 100_000), Mode: 0o644}},
		"many in a dir": manyFiles(3000),
	}
}

func manyFiles(n int) fstest.MapFS {
	tree := fstest.MapFS{}
	for i := range n {
		tree[fmt.Sprintf("d/%d", i)] = &fstest.MapFile{Data: fmt.Appendf(nil, "%d", i%7), Mode: 0o644 | fs.FileMode(i%2)*0o111}
	}

	return tree
}

// randomTree draws names from bytes a shell is likeliest to mishandle.
func randomTree(rng *rand.Rand, bytesSrc *rand.ChaCha8) fstest.MapFS {
	tree := fstest.MapFS{}
	dirs := []string{""}

	for range rng.IntN(25) {
		entry := path.Join(dirs[rng.IntN(len(dirs))], randomName(rng))

		if _, taken := tree[entry]; taken || isUnder(tree, entry) {
			continue
		}

		tree[entry] = randomFile(rng, bytesSrc)
		if tree[entry].Mode.IsDir() {
			dirs = append(dirs, entry)
		}
	}

	return tree
}

func randomName(rng *rand.Rand) string {
	alphabet := []string{"a", "b", "\n", " ", "-", "*", "\\", "'", "\"", "é", "$", "0a", "00", "\t", "."}

	var b strings.Builder
	for range 1 + rng.IntN(4) {
		b.WriteString(alphabet[rng.IntN(len(alphabet))])
	}

	if s := b.String(); s != "." && s != ".." {
		return s
	}

	return "z"
}

func randomFile(rng *rand.Rand, bytesSrc *rand.ChaCha8) *fstest.MapFile {
	switch rng.IntN(6) {
	case 0:
		return &fstest.MapFile{Mode: fs.ModeDir | 0o755}
	case 1:
		return &fstest.MapFile{Data: []byte(randomName(rng) + randomName(rng)), Mode: fs.ModeSymlink}
	case 2:
		return &fstest.MapFile{Mode: fs.ModeNamedPipe | 0o644}
	default:
		data := make([]byte, rng.IntN(64))
		_, _ = bytesSrc.Read(data)

		return &fstest.MapFile{Data: data, Mode: []fs.FileMode{0o644, 0o755, 0o600, 0o700, 0o601}[rng.IntN(5)]}
	}
}

// isUnder reports a non-directory already sitting on one of entry's parents.
func isUnder(tree fstest.MapFS, entry string) bool {
	for dir := path.Dir(entry); dir != "."; dir = path.Dir(dir) {
		if file, ok := tree[dir]; ok && !file.Mode.IsDir() {
			return true
		}
	}

	return false
}

func writeTree(t *testing.T, w *tar.Writer, root string, tree fstest.MapFS) {
	t.Helper()

	write := func(header *tar.Header, data []byte) {
		header.Format = tar.FormatGNU // GNU, not PAX: PAX refuses a name that is not UTF-8.
		header.ModTime = time.Unix(0, 0)

		err := w.WriteHeader(header)
		if err != nil {
			t.Fatalf("tar header %q: %v", header.Name, err)
		}

		_, err = w.Write(data)
		if err != nil {
			t.Fatalf("tar body %q: %v", header.Name, err)
		}
	}

	write(&tar.Header{Typeflag: tar.TypeDir, Name: root + "/", Mode: 0o755}, nil)

	// Map keys, not fs.WalkDir: a key that is not UTF-8 is a tree busybox must see even though MapFS cannot open it.
	written := map[string]bool{}

	for _, name := range slices.Sorted(maps.Keys(tree)) {
		for _, dir := range parentsOf(name) {
			if !written[dir] {
				written[dir] = true
				write(&tar.Header{Typeflag: tar.TypeDir, Name: root + "/" + dir + "/", Mode: 0o755}, nil)
			}
		}

		file := tree[name]
		if file.Mode.IsDir() && written[name] {
			continue
		}

		written[name] = true

		header := headerFor(root+"/"+name, file)
		if header.Typeflag == tar.TypeReg {
			write(header, file.Data)
		} else {
			write(header, nil)
		}
	}
}

// parentsOf lists name's directories, outermost first.
func parentsOf(name string) []string {
	var parents []string
	for dir := path.Dir(name); dir != "."; dir = path.Dir(dir) {
		parents = append(parents, dir)
	}

	slices.Reverse(parents)

	return parents
}

func headerFor(full string, file *fstest.MapFile) *tar.Header {
	switch {
	case file.Mode.IsDir():
		return &tar.Header{Typeflag: tar.TypeDir, Name: full + "/", Mode: 0o755}
	case file.Mode&fs.ModeSymlink != 0:
		return &tar.Header{Typeflag: tar.TypeSymlink, Name: full, Linkname: string(file.Data)}
	case file.Mode&fs.ModeNamedPipe != 0:
		return &tar.Header{Typeflag: tar.TypeFifo, Name: full, Mode: 0o644}
	default:
		return &tar.Header{Typeflag: tar.TypeReg, Name: full, Mode: int64(file.Mode.Perm()), Size: int64(len(file.Data))}
	}
}

// busyboxDigests runs Script once over every tree, in the pinned image, the way a worker would.
func busyboxDigests(t *testing.T, trees []fstest.MapFS, user string, prepare ...string) []string {
	t.Helper()

	var archive bytes.Buffer

	w := tar.NewWriter(&archive)

	err := w.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "digest.sh", Mode: 0o644, Size: int64(len(treedigest.Script))})
	if err != nil {
		t.Fatalf("tar: %v", err)
	}

	_, _ = w.Write([]byte(treedigest.Script))

	for i, tree := range trees {
		writeTree(t, w, fmt.Sprintf("t/%05d", i), tree)
	}

	err = w.Close()
	if err != nil {
		t.Fatalf("tar: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "-i", "--network", "none", "--user", user, //nolint:gosec // fixed image, test-built args
		treedigest.Image, "sh", "-c",
		`set -e; cd /tmp && tar xf - && `+strings.Join(append(prepare, "true"), " && ")+` && for d in t/*; do printf '%s ' "${d#t/}"; sh digest.sh "$d" || echo FAILED; done`)
	cmd.Stdin = &archive

	var stderr bytes.Buffer

	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("busybox: %v\n%s", err, stderr.String())
	}

	var sums []string

	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		_, sum, _ := strings.Cut(scanner.Text(), " ")
		sums = append(sums, sum)
	}

	if len(sums) != len(trees) {
		t.Fatalf("busybox printed %d digests for %d trees:\n%s\n%s", len(sums), len(trees), out, stderr.String())
	}

	return sums
}

// The worker and the orchestrator must name a tree alike, or a held output is never found again.
func TestScriptAgreesWithTree(t *testing.T) {
	requireDocker(t)

	const random = 300

	edges := edgeTrees()
	names := make([]string, 0, len(edges)+random)
	trees := make([]fstest.MapFS, 0, len(edges)+random)

	for name, tree := range edges {
		names = append(names, name)
		trees = append(trees, tree)
	}

	bytesSrc := rand.NewChaCha8([32]byte{2, 0, 6})
	rng := rand.New(rand.NewPCG(206, 1)) //nolint:gosec // reproducible inputs, not secrets

	for i := range random {
		names = append(names, fmt.Sprintf("random #%d", i))
		trees = append(trees, randomTree(rng, bytesSrc))
	}

	sums := busyboxDigests(t, trees, "0")

	failures := 0

	for i, tree := range trees {
		if want := digest(t, tree); sums[i] != want {
			t.Errorf("%s: busybox %s, Go %s; names %q", names[i], sums[i], want, slices.Sorted(maps.Keys(tree)))

			if failures++; failures == 5 {
				t.Fatal("stopping at five disagreements")
			}
		}
	}
}

// An entry the worker cannot read must fail the digest, never hash as absent.
func TestScriptFailsOnUnreadableFile(t *testing.T) {
	requireDocker(t)

	trees := []fstest.MapFS{
		{"secret": {Data: []byte("x"), Mode: 0o000}},
		{"d/x\n": {Data: []byte("x"), Mode: 0o000}},
		{"locked/f": {Data: []byte("x"), Mode: 0o644}},
	}

	for i, sum := range busyboxDigests(t, trees, "65534", "chmod 000 t/00002/locked") {
		if sum != "FAILED" {
			t.Errorf("tree %d: printed %q for an unreadable file", i, sum)
		}
	}
}

func TestTreeNamesWhatTheNextCommandSees(t *testing.T) {
	t.Parallel()

	base := func() fstest.MapFS {
		return fstest.MapFS{
			"bin/run":  {Data: []byte("#!/bin/sh\n"), Mode: 0o755},
			"src/a.go": {Data: []byte("package a"), Mode: 0o644},
			"src/link": {Data: []byte("a.go"), Mode: fs.ModeSymlink},
			"empty":    {Mode: fs.ModeDir | 0o755},
		}
	}

	same := digest(t, base())

	changes := map[string]func(fstest.MapFS){
		"exec bit dropped":    func(tree fstest.MapFS) { tree["bin/run"].Mode = 0o644 },
		"content":             func(tree fstest.MapFS) { tree["src/a.go"].Data = []byte("package b") },
		"renamed":             func(tree fstest.MapFS) { tree["src/b.go"] = tree["src/a.go"]; delete(tree, "src/a.go") },
		"link target":         func(tree fstest.MapFS) { tree["src/link"].Data = []byte("b.go") },
		"empty dir removed":   func(tree fstest.MapFS) { delete(tree, "empty") },
		"empty file added":    func(tree fstest.MapFS) { tree["src/new"] = &fstest.MapFile{Mode: 0o644} },
		"file became dir":     func(tree fstest.MapFS) { tree["empty"] = &fstest.MapFile{Mode: 0o644} },
		"link became file":    func(tree fstest.MapFS) { tree["src/link"].Mode = 0o644 },
		"file became fifo":    func(tree fstest.MapFS) { tree["src/a.go"] = &fstest.MapFile{Mode: fs.ModeNamedPipe} },
		"group exec only set": func(tree fstest.MapFS) { tree["src/a.go"].Mode = 0o654 },
	}
	for name, change := range changes {
		tree := base()
		change(tree)

		if digest(t, tree) == same {
			t.Errorf("%s: digest did not change", name)
		}
	}

	invariants := map[string]func(fstest.MapFS){
		"mtime":           func(tree fstest.MapFS) { tree["src/a.go"].ModTime = time.Unix(1e9, 0) },
		"read bits":       func(tree fstest.MapFS) { tree["src/a.go"].Mode = 0o600 },
		"exec bits moved": func(tree fstest.MapFS) { tree["bin/run"].Mode = 0o710 },
		"dir mode":        func(tree fstest.MapFS) { tree["empty"].Mode = fs.ModeDir | 0o700 },
	}
	for name, change := range invariants {
		tree := base()
		change(tree)

		if digest(t, tree) != same {
			t.Errorf("%s: digest changed", name)
		}
	}
}

// os.DirFS is what production walks; it must agree with the MapFS the busybox comparison is built from.
func TestTreeOnDiskMatchesTheSameTreeInMemory(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	for name, mode := range map[string]os.FileMode{"bin/run": 0o755, "a\nb": 0o644, "src/a.go": 0o600} {
		full := filepath.Join(root, filepath.FromSlash(name))

		err := os.MkdirAll(filepath.Dir(full), 0o750)
		if err != nil {
			t.Fatal(err)
		}

		err = os.WriteFile(full, []byte(name), mode)
		if err != nil {
			t.Fatal(err)
		}
	}

	err := os.Symlink("../a\nb", filepath.Join(root, "src", "link"))
	if err != nil {
		t.Fatal(err)
	}

	err = os.Mkdir(filepath.Join(root, "empty"), 0o750)
	if err != nil {
		t.Fatal(err)
	}

	memory := fstest.MapFS{
		"bin/run":  {Data: []byte("bin/run"), Mode: 0o755},
		"a\nb":     {Data: []byte("a\nb"), Mode: 0o644},
		"src/a.go": {Data: []byte("src/a.go"), Mode: 0o600},
		"src/link": {Data: []byte("../a\nb"), Mode: fs.ModeSymlink},
		"empty":    {Mode: fs.ModeDir | 0o755},
	}

	disk, err := treedigest.Tree(root)
	if err != nil {
		t.Fatal(err)
	}

	if mem := digest(t, memory); disk != mem {
		t.Fatalf("disk %s, memory %s", disk, mem)
	}
}

func TestTreeFailsOnUnreadableFile(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 file")
	}

	root := t.TempDir()

	err := os.WriteFile(filepath.Join(root, "secret"), []byte("x"), 0o000)
	if err != nil {
		t.Fatal(err)
	}

	_, err = treedigest.Tree(root)
	if err == nil || !strings.Contains(err.Error(), "secret") {
		t.Fatalf("want an error naming the file, got %v", err)
	}
}

// MapFS cannot hold a name that is not UTF-8, so busybox's answer is checked against the manifest written out by hand.
func TestScriptHexesBytesNotRunes(t *testing.T) {
	requireDocker(t)

	content := sha256.Sum256([]byte("x"))
	manifest := "fffe d\n" + "fffe2f80 f - " + hex.EncodeToString(content[:]) + "\n"
	want := sha256.Sum256([]byte(manifest))

	sums := busyboxDigests(t, []fstest.MapFS{{"\xff\xfe/\x80": {Data: []byte("x"), Mode: 0o644}}}, "0")
	if sums[0] != hex.EncodeToString(want[:]) {
		t.Fatalf("busybox %s, manifest %s", sums[0], hex.EncodeToString(want[:]))
	}
}

func TestRawFSHandsNamesToTheOS(t *testing.T) {
	t.Parallel()

	_, err := treedigest.RawFS(t.TempDir()).Open("\xff")
	if errors.Is(err, fs.ErrInvalid) {
		t.Fatalf("a name that is not UTF-8 was refused before the OS saw it: %v", err)
	}
}
