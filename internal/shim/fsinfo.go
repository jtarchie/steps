package shim

// fsInfo names the filesystem a path sits on, and the bytes available on it.
//
// A package variable for the same reason the venue's control-plane clients
// are: a machine has the filesystems it has, and a test cannot mount a second
// one to prove the shim measured the right path rather than a plausible
// neighbour. Substituting this is the only way to assert WHICH path was
// asked about -- on a single-filesystem machine, measuring "/" instead of the
// workdir returns an identical, and identically wrong, answer.
//
//nolint:gochecknoglobals // a test seam for a syscall, documented above
var fsInfo = fsInfoAt
