package shim

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/compress"
	"github.com/jtarchie/steps/internal/wire"
)

// FuzzCheckHello: an accepted hello puts the scratch exactly where its two halves say, so cleanup's remove-the-parent lands on this session's own directory and nothing above it.
func FuzzCheckHello(f *testing.F) {
	for _, seed := range [][2]string{
		{"s", ""}, {"s", "/tmp"}, {"../..", "/tmp"}, {"/", ""}, {"artifacts", ""}, {"s", "/tmp/../../etc"},
		{"s", "relative"}, {"s", "//abs//path/."}, {"a/b", "/tmp"}, {".", "/"}, {"s", "/./.."},
	} {
		f.Add(seed[0], seed[1])
	}

	f.Fuzz(func(t *testing.T, session, root string) {
		err := checkHello(wire.Hello{Session: session, Root: root})
		if err != nil {
			if !errors.Is(err, errBadSession) && !errors.Is(err, errBadRoot) {
				t.Fatalf("checkHello(%q, %q): %v is neither sentinel", session, root, err)
			}

			return
		}

		base := root
		if base == "" {
			base = "/default"
		}

		if !filepath.IsAbs(base) {
			t.Fatalf("accepted a relative root %q", root)
		}

		if filepath.Clean(base) != withoutDots(base) {
			t.Fatalf("root %q cleans to %q: it walks upward", root, filepath.Clean(base))
		}

		shims := filepath.Join(filepath.Clean(base), "steps-shim")
		workdir := filepath.Join(shims, session, "work")

		if filepath.Dir(filepath.Dir(workdir)) != shims {
			t.Fatalf("session %q puts the scratch at %q, not one level under %q", session, workdir, shims)
		}

		if filepath.Base(filepath.Dir(workdir)) == artifactCacheName {
			t.Fatalf("session %q is the artifact cache", session)
		}
	})
}

// FuzzCheckArtifact: each accepted half, joined onto its directory, stays exactly one level below it.
func FuzzCheckArtifact(f *testing.F) {
	for _, seed := range [][2]string{
		{"out", "abc123"}, {"../x", "d"}, {"o", "../../d"}, {"a/b", "d"}, {"", "d"}, {".", "d"}, {"o", "/"},
	} {
		f.Add(seed[0], seed[1])
	}

	f.Fuzz(func(t *testing.T, name, digest string) {
		err := checkArtifact(wire.UploadArtifact{Name: name, Digest: digest})
		if err != nil {
			if !errors.Is(err, errBadArtifact) {
				t.Fatalf("checkArtifact(%q, %q): %v is not errBadArtifact", name, digest, err)
			}

			return
		}

		for _, part := range []string{name, digest} {
			if !oneDirectoryName(part) {
				t.Fatalf("checkArtifact accepted %q, which oneDirectoryName refuses", part)
			}

			joined := filepath.Join("/work", part)
			if filepath.Dir(joined) != "/work" || joined == "/work" {
				t.Fatalf("%q joins to %q, not one level below", part, joined)
			}
		}
	})
}

// FuzzSameHostOnly: a redirect is followed only to the origin the request started at, and never past the hop bound.
func FuzzSameHostOnly(f *testing.F) {
	f.Add("https://bucket.s3.amazonaws.com/k?X-Amz-Signature=s", "https://bucket.s3.us-east-2.amazonaws.com/k", uint8(1))
	f.Add("https://a.example/k", "http://a.example/k", uint8(1))
	f.Add("https://a.example/k", "https://a.example/other", uint8(3))
	f.Add("https://a.example:443/k", "https://a.example/k", uint8(2))

	f.Fuzz(func(t *testing.T, first, next string, hops uint8) {
		firstURL, err := url.Parse(first)
		if err != nil {
			return
		}

		nextURL, err := url.Parse(next)
		if err != nil {
			return
		}

		via := make([]*http.Request, int(hops%6)+1)
		for i := range via {
			via[i] = &http.Request{URL: firstURL}
		}

		err = sameHostOnly(&http.Request{URL: nextURL}, via)
		if err != nil {
			if !errors.Is(err, errOffsiteRedirect) {
				t.Fatalf("%v is not errOffsiteRedirect", err)
			}

			if strings.Contains(err.Error(), "Signature") {
				t.Fatalf("the refusal leaks the query: %v", err)
			}

			return
		}

		if len(via) >= maxStoreRedirects {
			t.Fatalf("followed hop %d", len(via))
		}

		if nextURL.Host != firstURL.Host || nextURL.Scheme != firstURL.Scheme {
			t.Fatalf("followed %s://%s from %s://%s", nextURL.Scheme, nextURL.Host, firstURL.Scheme, firstURL.Host)
		}
	})
}

// FuzzUnpackVerified: a digest is a proof, so the only tree that is ever accepted is one whose bytes hash to the digest named, under either compression, and nothing lands beside the directory whatever the stream says.
func FuzzUnpackVerified(f *testing.F) {
	f.Add(fuzzTar(f, "out/a.txt", "alpha"), false, false)
	f.Add(fuzzTar(f, "out/a.txt", "alpha"), true, false)
	f.Add(fuzzTar(f, "../escaped", "x"), false, false)
	f.Add(fuzzTar(f, "out/a.txt", "alpha"), false, true)
	f.Add([]byte{}, false, false)
	f.Add([]byte("not a tar"), true, false)

	f.Fuzz(func(t *testing.T, stream []byte, zstd, lie bool) {
		digest := sha256.Sum256(stream)
		named := hex.EncodeToString(digest[:])

		if lie {
			named = strings.Repeat("0", len(named))
		}

		body := stream
		if zstd {
			var buf bytes.Buffer

			err := compress.Pack(&buf, true, func(w io.Writer) error {
				_, err := w.Write(stream)

				return err //nolint:wrapcheck // the test's own writer
			})
			if err != nil {
				t.Fatal(err)
			}

			body = buf.Bytes()
		}

		parent := t.TempDir()
		dir := filepath.Join(parent, "dir")

		err := os.Mkdir(dir, 0o700)
		if err != nil {
			t.Fatal(err)
		}

		err = unpackVerified(bytes.NewReader(body), dir, named, zstd)

		entries, readErr := os.ReadDir(parent)
		if readErr != nil || len(entries) != 1 {
			t.Fatalf("unpacking left %d entries beside the directory (%v)", len(entries), readErr)
		}

		defer openUp(dir)

		if err != nil {
			return
		}

		if lie {
			t.Fatal("accepted a stream under a digest it does not hash to")
		}

		if !hashesAsAPrefix(stream, named) {
			t.Fatalf("accepted under %s, which no block-aligned prefix of the stream hashes to", named)
		}
	})
}

// hashesAsAPrefix: tar reads whole 512-byte blocks and stops at the end marker, so what unpackVerified hashed is a block-aligned prefix of the stream or all of it.
func hashesAsAPrefix(stream []byte, digest string) bool {
	for end := 0; ; end += 512 {
		if end > len(stream) {
			end = len(stream)
		}

		sum := sha256.Sum256(stream[:end])
		if hex.EncodeToString(sum[:]) == digest {
			return true
		}

		if end == len(stream) {
			return false
		}
	}
}

func fuzzTar(f *testing.F, name, content string) []byte {
	f.Helper()

	var buf bytes.Buffer

	writer := tar.NewWriter(&buf)

	err := writer.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o644, Size: int64(len(content)), Format: tar.FormatPAX})
	if err == nil {
		_, err = writer.Write([]byte(content))
	}

	if err == nil {
		err = writer.Close()
	}

	if err != nil {
		f.Fatal(err)
	}

	return buf.Bytes()
}

// withoutDots is path with only its "." and empty elements dropped: what Clean gives when nothing walks upward.
func withoutDots(path string) string {
	var kept []string

	for _, element := range strings.Split(path, string(filepath.Separator)) {
		if element != "" && element != "." {
			kept = append(kept, element)
		}
	}

	return string(filepath.Separator) + strings.Join(kept, string(filepath.Separator))
}

// openUp restores write permission an archive's recorded modes took away, so t.TempDir can remove the tree.
func openUp(root string) {
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && entry.IsDir() {
			_ = os.Chmod(path, 0o700) //nolint:gosec // a directory this test owns
		}

		return nil
	})
}
