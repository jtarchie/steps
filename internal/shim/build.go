package shim

// Naming a binary by what it contains.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"sync"
)

// buildKeyBytes is how much of the hash names a binary. Half a sha256 is far
// past what a cache keyed by "which build is this" needs, and a shorter name
// is one somebody can compare by eye in an error message.
const buildKeyBytes = 16

// SelfBuild is the content hash of the running binary, memoized.
//
// Content rather than a version string, and the difference matters: the
// version is set by ldflags and is identical across every development build,
// so a cache keyed on it would keep executing a stale shim while somebody
// changed the code and wondered why nothing moved. A content hash cannot do
// that.
//
//nolint:gochecknoglobals // the answer cannot change while the process runs
var SelfBuild = sync.OnceValues(func() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locating this binary: %w", err)
	}

	return BuildOf(path)
})

// BuildOf is SelfBuild for a binary on disk, which is how the orchestrator
// names what it is about to push.
func BuildOf(path string) (string, error) {
	file, err := os.Open(path) //nolint:gosec // hashing a binary the caller named
	if err != nil {
		return "", fmt.Errorf("opening %q: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	sum := sha256.New()

	_, err = io.Copy(sum, file)
	if err != nil {
		return "", fmt.Errorf("hashing %q: %w", path, err)
	}

	return hex.EncodeToString(sum.Sum(nil))[:buildKeyBytes*2], nil
}
