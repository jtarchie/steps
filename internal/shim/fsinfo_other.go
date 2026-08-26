//go:build !linux && !darwin

package shim

// fsInfoAt has no answer on a platform with no statfs, and says so.
//
// Silence rather than a placeholder: a caller that requires a particular
// filesystem must refuse here, and a fabricated name would let it proceed.
func fsInfoAt(string) (string, uint64) {
	return "", 0
}
