package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
)

// Revision is one configuration, as parsed: the substituted source, the include contents it folded in, and the hash over both.
type Revision struct {
	SHA    string
	Source string
	// Includes is path→content, the path exactly as the pipeline names it: the string is hashed, so a resolved path made ci/app.yml and /repo/ci/app.yml two configurations of the same bytes.
	Includes map[string]string
}

// withIncludes hashes path→content PAIRS in sorted path order, so the hash describes the SET of includes rather than the order the resolver walked the config in.
func (r Revision) withIncludes(includes map[string]string) Revision {
	sum := sha256.New()
	sum.Write([]byte(r.Source))

	if len(includes) > 0 {
		for _, path := range slices.Sorted(maps.Keys(includes)) {
			sum.Write([]byte("\x00" + path + "\x00"))
			sum.Write([]byte(includes[path]))
		}

		r.Includes = includes
	}

	r.SHA = hex.EncodeToString(sum.Sum(nil))

	return r
}

// Recorded reports whether this Config was loaded from a source (rather than built in a test), which is what makes its revision worth writing down.
func (r Revision) Recorded() bool { return r.SHA != "" }

// ReadSource reads the pipeline file at path with ((name)) substitution applied: the bytes a revision is taken over, and what `steps pipeline set` uploads.
func ReadSource(path string, vars map[string]string) ([]byte, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is the pipeline file the user asked to run, not untrusted input
	if err != nil {
		return nil, fmt.Errorf("could not read pipeline file %q: %w", path, err)
	}

	return InterpolateVars(data, vars), nil
}

// Slugify turns a pipeline path into its identity: the base name without its
// extension.
//
// It lives here, and web.Slugify calls it, because a second copy is how the
// identity split the first time. internal/web cannot be imported from this
// package, and the string is needed on both sides.
func Slugify(path string) string {
	base := filepath.Base(path)

	return base[:len(base)-len(filepath.Ext(base))]
}
