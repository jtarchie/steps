//go:build !darwin && !linux

package workspace

// copyTreeCandidates returns a POSIX-portable cp invocation for platforms
// with no known copy-on-write cp flag (-c on darwin, --reflink on linux).
func copyTreeCandidates(src, dst string) [][]string {
	return [][]string{
		{"cp", "-R", "-P", "-p", src + "/.", dst},
	}
}
