//go:build darwin

package shim

import "syscall"

// fsInfoAt names the filesystem at path and how many bytes are available on it.
//
// Darwin needs no magic table: statfs reports the name directly, which is why
// this is a separate file rather than a build tag inside one.
func fsInfoAt(path string) (string, uint64) {
	var stat syscall.Statfs_t

	err := syscall.Statfs(path, &stat)
	if err != nil {
		return "", 0
	}

	// Widened to rune rather than narrowed to byte: Fstypename is [16]int8,
	// and int8 -> byte is a conversion that wraps for a negative, which a
	// filesystem name never is but nothing here proves.
	name := make([]rune, 0, len(stat.Fstypename))

	for _, char := range stat.Fstypename {
		if char == 0 {
			break
		}

		name = append(name, rune(char))
	}

	return string(name), stat.Bavail * uint64(stat.Bsize)
}
