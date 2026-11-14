package venue

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/dockerapi"
	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/treedigest"
	"github.com/jtarchie/steps/internal/wire"
)

// A holder's URL reaches a note a person reads, so its key paths stay out of it; an unparseable one is shown as it came.
func TestHolderAddressDropsConnectionOptions(t *testing.T) {
	t.Parallel()

	if got := holderAddress("docker+ssh://jt@box:2222?identity=/secret/key&sock=/run/docker.sock"); strings.Contains(got, "/secret/key") || !strings.Contains(got, "box:2222") {
		t.Errorf("holderAddress = %q, want the machine without its key path", got)
	}

	if got := holderAddress("::nonsense"); got != "::nonsense" {
		t.Errorf("holderAddress of an unparseable holder = %q, want it unchanged", got)
	}
}

// Two local: roots on one daemon are two caches; a bare local: keeps the unprefixed names.
func TestLocalRootsNameSeparateCaches(t *testing.T) {
	t.Parallel()

	alias := func(root string) string {
		return newPlusSession(Worker{Scheme: SchemeLocal, Root: root}, shell.RunnerSpec{}).aliasName("digest")
	}

	if alias("/a") == alias("/b") {
		t.Errorf("roots /a and /b both publish %q; they would share one cache", alias("/a"))
	}

	if got, want := alias(""), aliasName("digest"); got != want {
		t.Errorf("bare local: publishes %q, want %q", got, want)
	}
}

// Two sessions filling one digest race to publish it; the loser must be handed the winner's volume to mount, not nothing.
func TestDockerPlusALostPublishHandsBackTheWinner(t *testing.T) {
	hostDockerSocket(t)

	worker, err := ParseWorker("local:" + t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	s := newPlusSession(worker, shell.RunnerSpec{})
	t.Cleanup(func() { _ = s.close() })

	ctx := t.Context()

	_, err = s.dialDaemon(ctx)
	if err != nil {
		t.Fatal(err)
	}

	digest := randomSuffix()
	t.Cleanup(func() { _ = s.docker.RemoveVolume(context.Background(), s.aliasName(digest)) })

	first, second := dataVolume(t, s), dataVolume(t, s)

	won, err := s.publish(ctx, digest, first, 1)
	if err != nil || won.Name != first.Name {
		t.Fatalf("first publish = %q, %v; want its own volume", won.Name, err)
	}

	lost, err := s.publish(ctx, digest, second, 1)
	if err != nil || lost.Name != first.Name {
		t.Errorf("losing publish = %q, %v; want the winner %q", lost.Name, err, first.Name)
	}
}

func dataVolume(t *testing.T, s *plusSession) dockerapi.Volume {
	t.Helper()

	volume, err := s.docker.CreateVolume(t.Context(), "steps-d-"+randomSuffix(), shell.OwnershipLabels(), nil)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = s.docker.RemoveVolume(context.Background(), volume.Name) })

	return volume
}

// memo is a build's digest memo with one entry, recording what a venue taught it.
type memo struct {
	known   string
	learned string
}

func (m *memo) KnownDigest(string) (string, bool) { return m.known, m.known != "" }

func (m *memo) LearnDigest(_, digest string) { m.learned = digest }

// An input the build already digested is looked up by that digest, not walked again: the copy here differs from the published tree, so only a lookup that never read it mounts the published one and sends nothing.
func TestAKnownInputDigestIsPlacedWithoutWalkingTheCopy(t *testing.T) {
	hostDockerSocket(t)

	worker, err := ParseWorker("local:" + t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	known := &memo{known: randomSuffix()}
	s := newPlusSession(worker, shell.RunnerSpec{InputDigests: known})
	t.Cleanup(func() { _ = s.close() })

	ctx := t.Context()

	_, err = s.dialDaemon(ctx)
	if err != nil {
		t.Fatal(err)
	}

	published := t.TempDir()
	writeTreeFile(t, published, "a.txt", "published")

	digest, err := treedigest.Tree(published)
	if err != nil {
		t.Fatal(err)
	}

	known.known = digest
	t.Cleanup(func() { _ = s.docker.RemoveVolume(context.Background(), s.aliasName(digest)) })

	_, err = s.fill(ctx, digest, func(w io.Writer) error { return wire.PackTree(w, published) })
	if err != nil {
		t.Fatal(err)
	}

	sent := s.sent.Load()
	copyHere := t.TempDir()
	writeTreeFile(t, copyHere, "a.txt", "a copy that was never walked")

	_, err = s.placeLocal(ctx, "tree", copyHere)
	if err != nil {
		t.Fatal(err)
	}

	if got := s.sent.Load() - sent; got != 0 || known.learned != "" {
		t.Errorf("placing a known input sent %d bytes and learned %q; want a lookup by the known digest alone", got, known.learned)
	}
}

// A known digest the worker does not hold is a miss: the copy is walked, and its own digest learned, never filled under the remembered one.
func TestAKnownInputDigestTheWorkerLacksWalksTheCopy(t *testing.T) {
	hostDockerSocket(t)

	worker, err := ParseWorker("local:" + t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	known := &memo{known: randomSuffix()}
	s := newPlusSession(worker, shell.RunnerSpec{InputDigests: known})
	t.Cleanup(func() { _ = s.close() })

	_, err = s.dialDaemon(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	copyHere := t.TempDir()
	writeTreeFile(t, copyHere, "a.txt", "never published")

	want, err := treedigest.Tree(copyHere)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = s.docker.RemoveVolume(context.Background(), s.aliasName(want)) })

	_, err = s.placeLocal(t.Context(), "tree", copyHere)
	if err != nil {
		t.Fatal(err)
	}

	if known.learned != want {
		t.Errorf("learned %q after a miss; want the copy's own digest %q", known.learned, want)
	}
}

func writeTreeFile(t *testing.T, dir, name, body string) {
	t.Helper()

	err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600)
	if err != nil {
		t.Fatal(err)
	}
}
