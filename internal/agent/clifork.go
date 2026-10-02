package agent

// cliFork is a subagent the CLI forked that its own stream does not narrate
// (see recordCLIFork). Its calls are recorded by the bridge into rec, one
// level deeper than the step, and nested into the step's transcript as a
// delegation when it closes — the same shape a steps sub-agent leaves.
type cliFork struct {
	id      string
	name    string
	request string
	rec     *transcriptRecorder
}

// openFork starts recording a fork. One open at a time: a second
// announcement closes the first, since a CLI forks a typed command only
// before its first turn.
func (r *transcriptRecorder) openFork(id, name, request string) {
	if r == nil {
		return
	}

	r.closeFork("")

	fork := &cliFork{id: id, name: name, request: request, rec: r.childRecorder()}

	r.mu.Lock()
	r.fork = fork
	r.mu.Unlock()
}

// closeFork nests the open fork into the transcript. An empty id closes
// whichever fork is open; any other id closes only that one.
func (r *transcriptRecorder) closeFork(id string) {
	if r == nil {
		return
	}

	r.mu.Lock()

	fork := r.fork
	if fork == nil || (id != "" && fork.id != id) {
		r.mu.Unlock()

		return
	}

	r.fork = nil
	r.mu.Unlock()

	r.subagent(fork.name, fork.request, fork.rec.recorded())
}

// forkRecorder is the open fork's recorder, or nil when none is open — which
// every recorder method accepts, so a caller records into it unconditionally.
func (r *transcriptRecorder) forkRecorder() *transcriptRecorder {
	if r == nil {
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.fork == nil {
		return nil
	}

	return r.fork.rec
}
