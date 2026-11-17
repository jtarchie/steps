package resource

import (
	"archive/tar"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type unreachedTransport struct{ called bool }

func (u *unreachedTransport) RoundTrip(*http.Request) (*http.Response, error) {
	u.called = true

	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
}

// TestCleartextGuardRefusesALoginOverPlainHTTPOffThisMachine: go-containerregistry falls back to http for a private-network registry on its own; with a login in play that request never leaves.
func TestCleartextGuardRefusesALoginOverPlainHTTPOffThisMachine(t *testing.T) {
	t.Parallel()

	cases := map[string]bool{
		"http://10.0.0.5:5000/v2/":    false,
		"http://registry.lan/v2/":     false,
		"http://127.0.0.1:5050/v2/":   true,
		"http://localhost:5050/v2/":   true,
		"http://[::1]:5050/v2/":       true,
		"https://10.0.0.5:5000/v2/":   true,
		"https://ghcr.io/v2/owner/x/": true,
	}

	for raw, allowed := range cases {
		next := &unreachedTransport{}

		parsed, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}

		response, err := cleartextGuard{next: next}.RoundTrip(&http.Request{URL: parsed})
		if response != nil {
			_ = response.Body.Close()
		}
		if allowed != (err == nil) || allowed != next.called {
			t.Errorf("%s: err = %v, reached the network = %v; want allowed = %v", raw, err, next.called, allowed)
		}
	}
}

// TestInsideDirRefusesAPathOutOfThePutsTree: a params: path, or a symlink in the build's tree, cannot point the put at a file elsewhere on this machine.
func TestInsideDirRefusesAPathOutOfThePutsTree(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.tar")

	err := os.WriteFile(outside, []byte("x"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	err = os.MkdirAll(filepath.Join(dir, "image"), 0o700)
	if err != nil {
		t.Fatal(err)
	}

	err = os.Symlink(outside, filepath.Join(dir, "image", "image.tar"))
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(filepath.Join(dir, "image", "tags"), []byte("v1"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	for _, rel := range []string{"image/image.tar", "..", "../" + filepath.Base(filepath.Dir(outside)) + "/secret.tar"} {
		_, err := insideDir(dir, rel)
		if err == nil || !strings.Contains(err.Error(), "leads outside") {
			t.Errorf("insideDir(%q) = %v, want it refused", rel, err)
		}
	}

	_, err = insideDir(dir, "image/tags")
	if err != nil {
		t.Errorf("insideDir(image/tags) = %v, want a path in the tree allowed", err)
	}
}

// TestUntarLayoutRefusesWhatALayoutNeverHolds: a link, or a path climbing out of the unpack directory, is a tarball that is not an OCI layout.
func TestUntarLayoutRefusesWhatALayoutNeverHolds(t *testing.T) {
	t.Parallel()

	cases := map[string]tar.Header{
		"climbs":  {Name: "../escape", Typeflag: tar.TypeReg, Mode: 0o600},
		"symlink": {Name: "blobs", Typeflag: tar.TypeSymlink, Linkname: "/etc"},
	}

	for label, header := range cases {
		path := filepath.Join(t.TempDir(), "image.tar")

		file, err := os.Create(path) //nolint:gosec // a t.TempDir() path
		if err != nil {
			t.Fatal(err)
		}

		writer := tar.NewWriter(file)

		err = writer.WriteHeader(&header)
		if err != nil {
			t.Fatal(err)
		}

		_ = writer.Close()
		_ = file.Close()

		dst := t.TempDir()

		err = untarLayout(path, dst)
		if err == nil {
			t.Errorf("%s: unpacked, want refused", label)
		}

		_, statErr := os.Lstat(filepath.Join(filepath.Dir(dst), "escape"))
		if statErr == nil {
			t.Errorf("%s: wrote outside the unpack directory", label)
		}
	}
}
