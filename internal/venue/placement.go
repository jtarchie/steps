package venue

// What a placed step can say about the machine it ran on.

import "github.com/jtarchie/steps/internal/shell"

// Placement is the machine a placed step actually ran on, as that machine
// described itself.
//
// Facts, never a price. What an EC2 instance-hour costs is not something this
// process can honestly know — list prices ignore Savings Plans and Reserved
// Instances, a spot instance's paid price is reported by no API, and real
// billing lands up to a day later. What IS knowable here is which machine, of
// what shape, holding which filesystem, running which image, and how many
// bytes had to be pushed to it. That is also what somebody debugging a step
// that passed locally and failed placed needs, so the same record serves both.
//
// Everything but Tag and Address comes from the worker's own hello: this end
// asserts nothing about the far one. Instance is empty for every worker that
// is not an EC2 instance, and UID nil means the shim did not say — never
// "root", which is the common answer under the aws:// bootstrap and the
// opposite conclusion.
type Placement struct {
	Tag      string
	Address  string
	Instance string
	GOOS     string
	GOARCH   string
	Workdir  string
	FSType   string
	FSFree   uint64
	UID, GID *int
	Image    string
	// BytesSent is what this session put on the wire for step trees. It is
	// the number the artifact grain exists to reduce, and no vantage point
	// outside the session can weigh it — the tunnel is a pipe to a process.
	BytesSent int64
}

// PlacementOf describes the machine a runner used, once it has described
// itself.
//
// False until the handshake has happened, which is later than it looks: a
// session dials lazily, so a runner that was built and never asked to run
// anything has no facts to report and inventing them would record a machine
// nobody used. Ask a FINISHED runner — the byte count is only whole then, and
// a re-placed step reports the machine it ended on.
//
// A runner that is not placed answers false, so callers need not first ask
// whether they are holding a venue.
func PlacementOf(runner shell.Runner) (Placement, bool) {
	placed, ok := runner.(interface{ placement() (Placement, bool) })
	if !ok {
		return Placement{}, false
	}

	return placed.placement()
}

// placement is PlacementOf's implementation, unexported so the question is
// asked through the package function rather than by type-asserting a shape.
func (r runner) placement() (Placement, bool) { return r.session.placement() }

func (s *session) placement() (Placement, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// An empty workdir is a session that never completed a handshake: connect
	// refuses a hello without one (errNoWorkdir), so this is the single field
	// that separates "the worker told us about itself" from "we never got
	// that far".
	if s.workdir == "" {
		return Placement{}, false
	}

	return Placement{
		Tag:       s.tag,
		Address:   s.worker.Address(),
		Instance:  s.worker.Instance,
		GOOS:      s.goos,
		GOARCH:    s.goarch,
		Workdir:   s.workdir,
		FSType:    s.fstype,
		FSFree:    s.fsfree,
		UID:       s.uid,
		GID:       s.gid,
		Image:     s.container.Image,
		BytesSent: s.sentArtifactBytes.Load(),
	}, true
}
