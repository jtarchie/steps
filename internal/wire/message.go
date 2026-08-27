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
//
// 2 added FrameDraining, which is UNSOLICITED — unlike compression and the
// data plane, it is not something the hello can negotiate, because it arrives
// whenever the worker learns of its own end rather than in answer to
// anything. An older shim cannot be told to stay quiet about it and a newer
// one cannot be told to; the frame either exists for both ends or it kills a
// session mid-step with "unknown frame type". So it is a version, and a
// ?binary=-pinned shim from before it says so at the handshake.
// 4 made FrameUpload carry one entry per artifact rather than one blob for
// the whole tree, so a worker can skip fetching what it already holds. An
// older shim would read the new payload as an upload with no URL and fail
// the step rather than the session, which is a worse failure than refusing
// at the handshake.
//
// 3 added the docker stream frames. A shim that predates them answers an
// unknown frame type and kills the session mid-step, which is the same reason
// FrameDraining was a version rather than a negotiation: a frame either
// exists for both ends or it is a protocol error.
const Protocol = 4

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
	// DataPlane proposes how tree BYTES travel, separately from how frames
	// do. The one token is DataPlaneURLs: the orchestrator hands the shim a
	// URL per transfer and the tunnel carries control frames only. Same
	// degradation contract as Compression — the shim echoes what it accepts,
	// silence means the tunnel, and the tunnel is always the floor.
	DataPlane string `json:"data_plane,omitempty"`
}

// CompressionZstd is the one compression the protocol knows. The value is a
// name, not a family: a new algorithm is a new token both ends must learn,
// never a variation smuggled under this one.
const CompressionZstd = "zstd"

// DataPlaneURLs is tree transfer by presigned URL: the orchestrator PUTs the
// step tree to a blob store and sends a GET URL; the shim PUTs outputs to a
// URL it was handed. The worker needs no credentials beyond the URLs
// themselves, which is the entire design — a venue holds no AWS identity.
const DataPlaneURLs = "urls"

// Upload is FrameUpload's payload under DataPlaneURLs: where to fetch the
// step's tree, one entry per ARTIFACT rather than one blob for the whole
// tree. Absent (an empty payload) on the tunnel plane, where the tree follows
// as data frames.
//
// Per artifact because a whole-tree key never repeats. Two steps of one job
// sharing a large input still declare different outputs, so their trees
// differ by an empty directory and hash differently — measured: a 64MB input
// through three steps moved 192MB to the worker, the store deduplicating
// none of it. The shared bytes are in the artifact, so that is what is named.
type Upload struct {
	Artifacts []UploadArtifact `json:"artifacts,omitempty"`
}

// UploadArtifact is one top-level entry of a step's tree.
type UploadArtifact struct {
	// Name is the entry's name in the work directory.
	Name string `json:"name"`
	// Digest identifies the CONTENT, so a worker that already holds it can
	// say so instead of fetching it again.
	Digest string `json:"digest"`
	// URL fetches it, and is only used when the worker does not have it.
	URL string `json:"url"`
}

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
	// DataPlane is the transfer plane the shim accepted from Hello.DataPlane,
	// under the same take-it-or-leave-it contract.
	DataPlane string `json:"data_plane,omitempty"`
	// FSType names the filesystem Workdir sits on — "btrfs", "tmpfs", or the
	// hex magic when this shim has no name for that one. Reported because
	// only this end can see it, and it changes what a step COSTS rather than
	// what it means: a tmpfs workdir spends the machine's MEMORY on the
	// pushed binary and the step's tree, and loses both on a reboot.
	//
	// Empty means "this shim cannot say" — an older one, or a platform with
	// no answer — never "an ordinary disk". Anything that requires a
	// particular filesystem must fail closed on it.
	FSType string `json:"fstype,omitempty"`
	// FSFree is the bytes available on that filesystem, so a report can say
	// how much and not only what.
	FSFree uint64 `json:"fs_free,omitempty"`
	// UID and GID are the identity the shim runs as, for deciding which user
	// a container on this worker should write as. Pointers because zero is
	// ROOT and is the common answer under the aws:// bootstrap: a plain int
	// could not tell "runs as root" from "did not say", and those demand
	// opposite behaviour.
	//
	// Reported rather than assumed for the same reason Workdir is: a placed
	// containerized step bind-mounts a tree on the WORKER, so the identity
	// that matters is this end's, and the orchestrator's own is an answer
	// about a different machine.
	UID *int `json:"uid,omitempty"`
	GID *int `json:"gid,omitempty"`
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
	// URL, under DataPlaneURLs, is where the shim PUTs the packed outputs —
	// a presigned write the orchestrator minted for this one fetch. Empty on
	// the tunnel plane, where the outputs come back as data frames.
	URL string `json:"url,omitempty"`
}

// Draining is a worker announcing its own end: an eviction notice or a
// rebalance recommendation, seen by the shim and relayed before the machine
// goes away.
//
// Advisory, not fatal. A spot eviction gives about two minutes, so the
// command in flight may well finish; what this buys the orchestrator is
// knowing that a failure which follows is infrastructure rather than the
// step's own verdict.
type Draining struct {
	// Reason is what the worker learned, verbatim where possible — an
	// operator reading a build wants the words the cloud used.
	Reason string `json:"reason"`
	// Deadline is when the worker expects to be gone, RFC3339, empty when the
	// notice carried no time. Reported to the operator with the reason; this
	// end makes no scheduling decision from it, since the only question that
	// matters — is the machine definitely going — is Terminal's.
	Deadline string `json:"deadline,omitempty"`
	// Terminal separates a machine that IS going away from one that merely
	// might. EC2 says both through the same metadata service: a spot
	// instance-action is a decision already taken, while a rebalance
	// recommendation is a hint that need never be followed by anything.
	//
	// Carried explicitly because the two must not be treated alike. Acting on
	// an advisory the way this end acts on a reclamation would terminate a
	// healthy machine the job paid a minute to acquire, and re-run a step
	// that had nothing wrong with it.
	Terminal bool `json:"terminal"`
}

// Error reports a failed operation without ending the session.
type Error struct {
	Message string `json:"message"`
}
