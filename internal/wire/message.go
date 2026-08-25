package wire

// The control frames' payloads.
//
// Every field here answers a question one end cannot answer for itself. Where
// a question has a local answer — what the environment allowlist is, how much
// output to keep, whether to prefix a line with a label — it stays local, and
// there is deliberately no field for it.

// Protocol is the version both ends must agree on exactly.
//
// There is no negotiation and no compatibility matrix, because there is
// nothing to negotiate with: the shim is the binary the orchestrator itself
// pushed, keyed by that binary's own content hash. Two different versions mean
// somebody is pointing at a stale or foreign binary, and the useful response
// is to say so with both hashes rather than to degrade.
const Protocol = 1

// Hello opens a session.
type Hello struct {
	Protocol int `json:"protocol"`
	// Build is the content hash of the binary the orchestrator pushed. The
	// shim answers with its own, which catches a truncated upload as well as a
	// stale cache entry.
	Build string `json:"build"`
	// Session names the scratch directory, so two runs against one worker
	// cannot collide and a leftover directory says which run left it.
	Session string `json:"session"`
	// Keep leaves the scratch behind when the session ends, backing
	// --keep-workspace. Without it, the postmortem the flag exists for stops
	// at the machine boundary.
	Keep bool `json:"keep"`
	// Root is where the shim makes its scratch, from the worker URL's path.
	// Empty takes the shim's own default. Sent rather than assumed, because
	// only this end knows the mapping: a shim started by an operator over a
	// bare ssh command has no URL to read it from.
	Root string `json:"root,omitempty"`
	// Compression proposes how upload and fetch data payloads are encoded.
	// The shim echoes what it accepts in HelloOK.Compression, and silence —
	// an older shim that never learned the field — means raw, so a mixed
	// pair degrades to uncompressed rather than to a refusal. Negotiated
	// rather than versioned: the tar stream inside is identical either way,
	// so this is a transfer detail, not a protocol revision.
	Compression string `json:"compression,omitempty"`
}

// CompressionZstd is the one compression the protocol knows. The value is a
// name, not a family: a new algorithm is a new token both ends must learn,
// never a variation smuggled under this one.
const CompressionZstd = "zstd"

// HelloOK is the shim's answer.
type HelloOK struct {
	Protocol int    `json:"protocol"`
	Build    string `json:"build"`
	GOOS     string `json:"goos"`
	GOARCH   string `json:"goarch"`
	// Workdir is the absolute path the step's tree lands in, reported rather
	// than dictated so the orchestrator can name it in an error and in
	// --keep-workspace output.
	Workdir string `json:"workdir"`
	// Compression is the encoding the shim accepted from Hello.Compression:
	// the same token back, or empty for raw. Never a counter-proposal — the
	// orchestrator offers what it speaks, and the shim takes it or leaves it.
	Compression string `json:"compression,omitempty"`
}

// Exec asks for one command.
type Exec struct {
	// Command is a shell string, run exactly as HostRunner runs one. The
	// venue's whole promise is that placement does not change meaning, so this
	// crosses uninterpreted and the far end hands it to the same `sh -c`.
	Command string `json:"command"`
	// Env carries the VALUES of the variables the pipeline's env: opted into.
	//
	// Names-only is the rule everywhere the merkle map can see, because that
	// map is written to the state database. This is not that: the hash is
	// computed on the orchestrator, from the names, long before any of this is
	// sent. A worker cannot resolve a name against a shell it does not have,
	// so values travel — inside the session, never in an argv on either end,
	// which is the same property `docker -e NAME` was chosen for.
	//
	// The baseline is NOT sent. PATH, HOME and TMPDIR from the orchestrator
	// are meaningless on another machine and often actively wrong; the shim
	// resolves those from its own environment with the same allowlist.
	Env map[string]string `json:"env,omitempty"`
}

// Exit ends a command.
type Exit struct {
	// Started distinguishes a command that ran and failed from one that never
	// launched — a bad working directory, a missing interpreter, an
	// unreachable machine.
	//
	// os/exec expresses this by the type of the error it returns, which is
	// exactly what cannot cross a machine boundary, so it becomes a field.
	// Losing it would let an infrastructure outage read as a step's own
	// verdict: a guard command that could not run would be recorded as a guard
	// that said no, and the work it gates would be skipped silently.
	Started bool `json:"started"`
	// Code is the exit status, or -1 for a signalled command — the same
	// sentinel os/exec reports for a local kill, so the two are
	// indistinguishable to everything downstream.
	Code int `json:"code"`
	// Reason says why a command never started. Empty when Started.
	Reason string `json:"reason,omitempty"`
}

// Fetch asks for named subtrees of the work directory back.
//
// Named rather than whole-tree: a step declares its outputs, and shipping a
// two-gigabyte input back to prove it did not change is the difference between
// a feature that works and one anybody uses.
type Fetch struct {
	Paths []string `json:"paths"`
}

// Error reports a failed operation without ending the session.
type Error struct {
	Message string `json:"message"`
}
