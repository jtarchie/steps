package workspace

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"

	"github.com/jtarchie/steps/internal/treedigest"
)

// digestTree is treedigest's, the one name a tree has here and on every worker: a step whose input was local last build and held by a worker this build keys alike.
func digestTree(root string) (string, error) {
	digest, err := treedigest.Tree(root)
	if err != nil {
		return "", fmt.Errorf("digesting %q: %w", root, err)
	}

	return digest, nil
}

// digestField writes a length-prefixed variable-length field, so no arrangement of fields can produce another key's byte stream.
func digestField(sum hash.Hash, field []byte) {
	digestLength(sum, uint64(len(field)))
	_, _ = sum.Write(field)
}

func digestLength(sum hash.Hash, n uint64) {
	var prefix [8]byte

	binary.BigEndian.PutUint64(prefix[:], n)
	_, _ = sum.Write(prefix[:])
}

func hashHex(sum hash.Hash) string {
	return hex.EncodeToString(sum.Sum(nil))
}
