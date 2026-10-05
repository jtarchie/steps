// Package wire is the tree codec: a step's directory as a tar stream, packed and unpacked the way every transfer of one agrees on, symlinks kept and never followed, and nothing written outside the tree on the way in. Stdlib-only, so nothing it depends on can change what those bytes mean.
package wire
