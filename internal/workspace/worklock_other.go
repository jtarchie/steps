//go:build !darwin && !linux

package workspace

import "errors"

// tryLock has no flock to hold here, so every step takes a throwaway path.
func tryLock(string) (func(), error) {
	return nil, errors.New("no file locks on this platform")
}
