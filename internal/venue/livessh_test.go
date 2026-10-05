package venue

import (
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/jtarchie/steps/internal/testsshd"
)

// The retry after a dropped connection can arrive before the connection reports its end; a suspect one is asked, and a dead one answers by failing.
func TestLiveSSHAsksASuspectConnectionWhetherItIsAlive(t *testing.T) {
	t.Parallel()

	live := testsshd.New(t).Dial(t)
	dead := testsshd.New(t).Dial(t)
	_ = dead.Close()

	notYetEnded := make(chan struct{})

	for name, c := range map[string]struct {
		client         *ssh.Client
		suspect, ended bool
	}{
		"dead, suspect":     {client: dead, suspect: true, ended: true},
		"dead, not suspect": {client: dead},
		"live, suspect":     {client: live, suspect: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			l := &liveSSH{client: c.client}
			l.suspect.Store(c.suspect)

			if got := l.gone(notYetEnded); got != c.ended {
				t.Errorf("gone = %v, want %v", got, c.ended)
			}
		})
	}
}
