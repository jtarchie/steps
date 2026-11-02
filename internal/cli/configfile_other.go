//go:build !unix

package cli

import "os"

// checkOwnedByUs has no owner to compare on this platform; the mode check still applies.
func checkOwnedByUs(string, os.FileInfo) error { return nil }
