//go:build darwin

package main

// copyTreeCandidates returns cp invocations to copy src's contents into an
// existing dst, in preference order. -c requests APFS clonefile
// (copy-on-write) and errors on filesystems that don't support it (e.g.
// exfat, or a non-APFS network mount), so the plain form is the fallback.
// -P never follows symlinks (copies the link itself); -p preserves
// mode/timestamps.
func copyTreeCandidates(src, dst string) [][]string {
	return [][]string{
		{"cp", "-c", "-R", "-P", "-p", src + "/.", dst},
		{"cp", "-R", "-P", "-p", src + "/.", dst},
	}
}
