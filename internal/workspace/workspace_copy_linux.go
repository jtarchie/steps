//go:build linux

package workspace

// copyTreeCandidates returns cp invocations to copy src's contents into an
// existing dst. --reflink=auto requests a copy-on-write reflink where the
// filesystem supports it (e.g. btrfs, XFS with reflink=1) and transparently
// falls back to a plain copy otherwise, so a single candidate covers both
// cases. -a implies -dR --preserve=all (never follow symlinks, preserve
// mode/timestamps/ownership where possible).
func copyTreeCandidates(src, dst string) [][]string {
	return [][]string{
		{"cp", "-a", "--reflink=auto", src + "/.", dst},
	}
}
