//go:build !darwin

package workspace

import "errors"

// cloneTree has no whole-directory clone off macOS; cp --reflink=auto is the copy-on-write path there.
func cloneTree(string, string) error { return errors.ErrUnsupported }
