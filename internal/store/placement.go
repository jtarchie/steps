package store

// Placement is one placed step's record of the machine that ran it.
//
// InstanceID, UID and GID are pointers because absent and zero are different
// answers. Only an aws:// worker has an instance at all; only a shim that can
// answer reports an identity, and uid 0 is ROOT — the common case under the
// aws:// bootstrap — so an int could not tell "ran as root" from "did not
// say", and those mean opposite things to a reader.
type Placement struct {
	RunID     string
	StepIndex int
	StepName  string
	JobName   string
	// Slot is what makes this placement unique within its run: a plan step's
	// node hash, or a hook's scope label. A hook is not merkle-hashed, so it
	// has no node to be keyed on.
	Slot string
	// NodeHash is the node this placement belongs to, EMPTY for a hook. It is
	// what lets retention reaping a node take the placement with it; a hook
	// row has no node and cascades off its run instead.
	//
	// A string rather than a pointer, unlike InstanceID and UID beside it: a
	// hash is never legitimately empty, so there is no zero value to confuse
	// with absence, and a node's parent hash already spells the same
	// distinction this way.
	NodeHash   string
	Tag        string
	Address    string
	InstanceID *string
	GOOS       string
	GOARCH     string
	Workdir    string
	FSType     string
	FSFree     int64
	UID        *int
	GID        *int
	Image      string
	BytesSent  int64
}
