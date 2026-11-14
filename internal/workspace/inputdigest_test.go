package workspace

// A venue placing an input in a container digests it to look it up; the build's memo lets the next step reading the same bytes skip that walk (steps#246). These pin when the memo may answer: only for bytes nothing has replaced since.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

// inputDigests is shell.InputDigests, which this package may not import: a venue finds the memo by these two methods alone.
type inputDigests interface {
	KnownDigest(name string) (string, bool)
	LearnDigest(name, digest string)
}

func memoBuild(t *testing.T) BuildWorkspace {
	t.Helper()

	provider, err := NewProvider(&config.WorkspaceConfig{Strategy: "copy"}, false)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = provider.Close() })

	bw, err := provider.NewBuild(context.Background(), "build")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = bw.Close() })

	return bw
}

func memoSpace(t *testing.T, bw BuildWorkspace, label string) inputDigests {
	t.Helper()

	space, err := bw.TaskSpace(context.Background(), label, []string{"tree"}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = space.Close() })

	digests, ok := space.(inputDigests)
	if !ok {
		t.Fatal("an isolating space does not offer the build's digest memo")
	}

	return digests
}

func writeAndCapture(t *testing.T, bw BuildWorkspace, label string, inputs []string, body string) {
	t.Helper()

	space, err := bw.TaskSpace(context.Background(), label, inputs, []string{"tree"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = space.Close() }()

	err = os.WriteFile(filepath.Join(space.Dir(), "tree", "file.txt"), []byte(body), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	err = space.Capture(context.Background())
	if err != nil {
		t.Fatal(err)
	}
}

func TestADigestLearnedOfAnInputAnswersTheNextStepReadingIt(t *testing.T) {
	t.Parallel()

	bw := memoBuild(t)
	writeAndCapture(t, bw, "seed", nil, "aaaa")

	first := memoSpace(t, bw, "first")
	if _, ok := first.KnownDigest("tree"); ok {
		t.Fatal("a digest nobody computed was known")
	}

	first.LearnDigest("tree", "digest-of-aaaa")

	second := memoSpace(t, bw, "second")
	if got, ok := second.KnownDigest("tree"); !ok || got != "digest-of-aaaa" {
		t.Fatalf("second reader's known digest = %q, %v; want the one the first learned", got, ok)
	}

	if _, ok := second.KnownDigest("other"); ok {
		t.Error("a name that is not an input was known")
	}

	// Same size, same second: only the capture can tell the memo the bytes changed.
	writeAndCapture(t, bw, "edit", []string{"tree"}, "bbbb")

	third := memoSpace(t, bw, "third")
	if got, ok := third.KnownDigest("tree"); ok {
		t.Errorf("after a capture replaced the artifact, known digest = %q; want none", got)
	}

	// A space copied before the capture must not teach the memo its stale bytes' digest.
	second.LearnDigest("tree", "digest-of-aaaa")

	if got, ok := third.KnownDigest("tree"); ok {
		t.Errorf("a digest learned of a copy older than the artifact was kept: %q", got)
	}

	// Nor may the new bytes' digest answer for a copy of the old ones.
	third.LearnDigest("tree", "digest-of-bbbb")

	if got, ok := second.KnownDigest("tree"); ok {
		t.Errorf("a copy of the old bytes was answered with the new digest %q", got)
	}
}
