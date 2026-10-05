package wire

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FuzzUnpackName: whatever the name, an accepted one is a local path.
func FuzzUnpackName(f *testing.F) {
	for _, seed := range []string{"a", "a/b/", "../x", "/abs", "a/../../x", "..", "./a", "a//b", ""} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, name string) {
		clean, err := unpackName(name)
		if err != nil {
			if !errors.Is(err, ErrUnsafePath) {
				t.Fatalf("unpackName(%q): %v is not ErrUnsafePath", name, err)
			}

			return
		}

		if filepath.IsAbs(clean) || escapes(clean) {
			t.Fatalf("unpackName(%q) accepted %q", name, clean)
		}
	})
}

// FuzzCheckLinkTarget: an accepted target, joined onto the link's own directory, stays inside the tree.
func FuzzCheckLinkTarget(f *testing.F) {
	for _, seed := range [][2]string{
		{"a", "b"}, {"a/b", "../c"}, {"a", "../x"}, {"a", "/etc"}, {"a/b/c", "../../.."}, {"a", ""}, {"a", "./."},
	} {
		f.Add(seed[0], seed[1])
	}

	f.Fuzz(func(t *testing.T, name, target string) {
		err := checkLinkTarget(name, target)
		if err != nil {
			if !errors.Is(err, ErrUnsafeLink) {
				t.Fatalf("checkLinkTarget(%q, %q): %v is not ErrUnsafeLink", name, target, err)
			}

			return
		}

		resolved := filepath.Join(filepath.Dir(filepath.FromSlash(name)), filepath.FromSlash(target))
		if filepath.IsAbs(target) || escapes(resolved) {
			t.Fatalf("checkLinkTarget(%q, %q) accepted a link to %q", name, target, resolved)
		}
	})
}

// FuzzUnpackTree builds an archive from a fuzzed list of entries — raw bytes almost never parse as tar — and holds both unpackers to their contracts: nothing is ever written beside the tree, and a fetched tree's symlinks all land inside it however they chain through one another.
func FuzzUnpackTree(f *testing.F) {
	f.Add([]byte{0, 1, 2}, "d\nd/f\nd/l", "\n\nf", []byte("x"))
	f.Add([]byte{2, 1}, "link\nlink/escaped", "..\n", []byte("x"))
	f.Add([]byte{2, 2}, "a\na/b", "..\n../x", []byte{})
	f.Add([]byte{0, 2, 2}, "a\na/b\na/b/c", "\n..\n../x", []byte{})
	f.Add([]byte{0, 2, 2, 2}, "sub\nsub/deep\nb\na", "\n..\nsub/deep\nb/../outside", []byte{})
	f.Add([]byte{3}, "hard", "/etc/passwd", []byte{})
	f.Add([]byte{1}, "../escaped", "", []byte("x"))

	f.Fuzz(func(t *testing.T, kinds []byte, names, links string, body []byte) {
		archive := fuzzArchive(kinds, names, links, body)
		if archive == nil {
			return
		}

		unpackBeside(t, archive, false)
		unpackBeside(t, archive, true)
	})
}

// unpackBeside unpacks into a directory with a sentinel next to it, and fails if the sentinel or the directory listing around the tree changed.
func unpackBeside(t *testing.T, archive []byte, fetched bool) {
	t.Helper()

	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	sentinel := filepath.Join(parent, "sentinel")

	err := os.Mkdir(root, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	defer makeRemovable(root)

	err = os.WriteFile(sentinel, []byte("untouched"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	if fetched {
		err = UnpackFetchedTree(bytes.NewReader(archive), root)
	} else {
		err = UnpackTree(bytes.NewReader(archive), root)
	}

	entries, readErr := os.ReadDir(parent)
	if readErr != nil || len(entries) != 2 {
		t.Fatalf("fetched=%v: unpacking left %d entries beside the tree (%v)", fetched, len(entries), readErr)
	}

	content, readErr := os.ReadFile(sentinel) //nolint:gosec // a path this test made
	if readErr != nil || string(content) != "untouched" {
		t.Fatalf("fetched=%v: the file beside the tree changed: %q, %v", fetched, content, readErr)
	}

	if fetched && err == nil {
		assertLinksStayInside(t, root)
	}
}

// FuzzPackTreeRoundTrip builds a tree on disk, sends it through the codec, and requires the far side to hold what digestTree hashes: the same paths, kinds, executable bits, bytes and link targets. It also pins that packing is reproducible.
func FuzzPackTreeRoundTrip(f *testing.F) {
	f.Add([]byte{0, 1, 2, 5}, "d\nd/f\nd/l\nx", "\n\n../abs/elsewhere\n", []byte("body"))
	f.Add([]byte{1, 4}, "a\nb", "", []byte{})

	f.Fuzz(func(t *testing.T, kinds []byte, names, links string, body []byte) {
		from := t.TempDir()
		if !fuzzTree(from, kinds, names, links, body) {
			return
		}

		var first, second bytes.Buffer

		err := PackTree(&first, from)
		if err != nil {
			t.Fatalf("PackTree: %v", err)
		}

		err = PackTree(&second, from)
		if err != nil {
			t.Fatalf("PackTree again: %v", err)
		}

		if !bytes.Equal(first.Bytes(), second.Bytes()) {
			t.Fatal("packing the same tree twice produced different bytes")
		}

		to := t.TempDir()

		err = UnpackTree(&first, to)
		if err != nil {
			t.Fatalf("UnpackTree of a PackTree stream: %v", err)
		}

		want, got := treeFacts(t, from), treeFacts(t, to)
		if len(want) != len(got) {
			t.Fatalf("round trip has %d entries, want %d:\n%v\n%v", len(got), len(want), got, want)
		}

		for name, fact := range want {
			if got[name] != fact {
				t.Fatalf("%s: round trip gives %q, want %q", name, got[name], fact)
			}
		}
	})
}

const maxFuzzEntries = 16

// fuzzArchive turns three parallel lists into a tar stream. nil means the input could not be written, which is the tar writer refusing, not a finding.
func fuzzArchive(kinds []byte, names, links string, body []byte) []byte {
	if len(kinds) == 0 {
		return nil
	}

	nameList := strings.Split(names, "\n")
	linkList := strings.Split(links, "\n")

	var buf bytes.Buffer

	writer := tar.NewWriter(&buf)

	for i, name := range nameList {
		if i == maxFuzzEntries {
			break
		}

		link := ""
		if i < len(linkList) {
			link = linkList[i]
		}

		header := fuzzHeader(kinds[i%len(kinds)], name, link, len(body))

		if writer.WriteHeader(header) != nil {
			return nil
		}

		if header.Typeflag == tar.TypeReg {
			_, err := writer.Write(body)
			if err != nil {
				return nil
			}
		}
	}

	if writer.Close() != nil {
		return nil
	}

	return buf.Bytes()
}

func fuzzHeader(kind byte, name, link string, size int) *tar.Header {
	header := &tar.Header{Name: name, Mode: 0o755, Format: tar.FormatPAX}

	switch kind % 4 {
	case 0:
		header.Typeflag = tar.TypeDir
	case 1:
		header.Typeflag = tar.TypeReg
		header.Size = int64(size)
	case 2:
		header.Typeflag = tar.TypeSymlink
		header.Linkname = link
	default:
		header.Typeflag = tar.TypeLink
		header.Linkname = link
	}

	return header
}

// fuzzTree makes a tree from the same three lists, keeping to what PackTree can carry: local names, three kinds, and modes it reads back. False means nothing usable came out of the input.
func fuzzTree(root string, kinds []byte, names, links string, body []byte) bool {
	if len(kinds) == 0 {
		return false
	}

	linkList := strings.Split(links, "\n")
	made := false

	for i, name := range strings.Split(names, "\n") {
		if i == maxFuzzEntries {
			break
		}

		link := ""
		if i < len(linkList) {
			link = linkList[i]
		}

		made = makeFuzzEntry(root, kinds[i%len(kinds)], name, link, body) || made
	}

	return made
}

func makeFuzzEntry(root string, kind byte, name, link string, body []byte) bool {
	if !filepath.IsLocal(name) || strings.ContainsRune(name, 0) {
		return false
	}

	path := filepath.Join(root, name)
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return false
	}

	switch kind % 3 {
	case 0:
		return os.Mkdir(path, []fs.FileMode{0o700, 0o755, 0o750}[kind/3%3]) == nil
	case 1:
		return os.WriteFile(path, body, []fs.FileMode{0o600, 0o644, 0o755}[kind/3%3]) == nil
	default:
		return link != "" && !strings.ContainsRune(link, 0) && os.Symlink(link, path) == nil
	}
}

// treeFacts is what digestTree hashes, per relative path.
func treeFacts(t *testing.T, root string) map[string]string {
	t.Helper()

	facts := map[string]string{}

	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, _ := filepath.Rel(root, path)
		if rel == "." {
			return nil
		}

		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("%w", err)
		}

		switch {
		case entry.IsDir():
			facts[rel] = "dir " + info.Mode().Perm().String()
		case entry.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return fmt.Errorf("%w", err)
			}

			facts[rel] = "link " + target
		default:
			content, err := os.ReadFile(path) //nolint:gosec // a path the walk found
			if err != nil {
				return fmt.Errorf("%w", err)
			}

			facts[rel] = "file " + info.Mode().Perm().String() + " " + string(content)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}

	return facts
}

// assertLinksStayInside resolves every symlink in root the way the kernel would — through the links it meets on the way, not lexically — and fails if any lands outside root.
func assertLinksStayInside(t *testing.T, root string) {
	t.Helper()

	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}

	err = filepath.WalkDir(realRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.Type()&fs.ModeSymlink == 0 {
			return err
		}

		lands, reachable := resolveLink(path)
		if reachable && lands != realRoot && !strings.HasPrefix(lands, realRoot+string(filepath.Separator)) {
			rel, _ := filepath.Rel(realRoot, path)
			target, _ := os.Readlink(path)
			t.Fatalf("fetched symlink %s -> %s lands at %s, outside the tree", rel, target, lands)
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// resolveLink follows the link at path, component by component, to where an open through it would land. A name that does not exist can still be CREATED through the link when it is the last component, so that counts; anything past a missing component is unreachable.
func resolveLink(path string) (string, bool) {
	target, err := os.Readlink(path)
	if err != nil {
		return "", false
	}

	current := anchor(filepath.Dir(path), target)
	pending := splitPath(target)

	for hops := 0; len(pending) > 0; {
		part := pending[0]
		pending = pending[1:]

		if part == ".." {
			current = filepath.Dir(current)
		}

		if part == "." || part == ".." {
			continue
		}

		next := filepath.Join(current, part)

		inner, isLink, exists := readLinkAt(next)
		if !exists {
			return next, len(pending) == 0
		}

		if !isLink {
			current = next

			continue
		}

		hops++
		if hops > 40 {
			return "", false
		}

		current = anchor(current, inner)
		pending = append(splitPath(inner), pending...)
	}

	return current, true
}

func anchor(current, target string) string {
	if filepath.IsAbs(target) {
		return string(filepath.Separator)
	}

	return current
}

func readLinkAt(path string) (string, bool, bool) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", false, false
	}

	if info.Mode()&fs.ModeSymlink == 0 {
		return "", false, true
	}

	target, err := os.Readlink(path)

	return target, err == nil, err == nil
}

func splitPath(path string) []string {
	var parts []string

	for _, part := range strings.Split(path, string(filepath.Separator)) {
		if part != "" {
			parts = append(parts, part)
		}
	}

	return parts
}

// escapes reports a relative path that climbs out of wherever it is joined.
func escapes(path string) bool {
	clean := filepath.Clean(path)

	return clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

// makeRemovable opens up whatever modes an archive restored, so t.TempDir's cleanup can delete the tree.
func makeRemovable(root string) {
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && entry.IsDir() {
			_ = os.Chmod(path, 0o700) //nolint:gosec // a directory this test owns
		}

		return nil
	})
}
