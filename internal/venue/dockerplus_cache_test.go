package venue

import (
	"context"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/dockerapi"
	"github.com/jtarchie/steps/internal/shell"
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
