package shell

import (
	"crypto/sha256"
	"encoding/hex"
)

// A container step's tree is laid out as Concourse lays it out (steps#247): a path that belongs to the container, the same on every worker, naming nothing on the machine that started it.
const (
	buildRoot = "/tmp/build"
	// GetDir is where a resource's in: runs, the fetched artifact itself.
	GetDir = buildRoot + "/get"
	// PutDir is where a resource's out: runs, each input a directory beneath it.
	PutDir = buildRoot + "/put"
)

// BuildDir is where the step called name works: /tmp/build/ and the first four bytes of the name's sha256, which is Concourse's rule. Fixed by the name and by nothing else, deliberately — Concourse tried a path that changed every build and took it back, and measured here a moving path recompiles a whole Go module and re-runs every cached test, because Go keys both on the absolute source directory. A cache volume is scoped by the same name, so everything that reuses one sees the same path.
func BuildDir(name string) string {
	sum := sha256.Sum256([]byte(name))

	return buildRoot + "/" + hex.EncodeToString(sum[:4])
}
