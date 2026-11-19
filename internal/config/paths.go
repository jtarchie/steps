package config

// Path confinement: whether a pipeline-written path stays inside the tree it
// is joined under. load_var: file:, across: from_file:, an agent's dir: and
// context_paths: and its file tools all ask ConfinedPath; a capture's
// destination asks ValidateArtifactPath.

import (
	"fmt"
	"path"
	"strings"
)

// ConfinedPath joins rel onto dir and reports whether the cleaned result is
// dir itself or under it. An absolute rel is taken as it is, so it is inside
// only when it already names a place under an absolute dir.
//
// It is the one lexical confinement predicate: internal/agent's file tools
// and context_paths: ask it at run time, and load asks it of a
// workspace-relative path ("." for the step root), which is why it lives here
// rather than beside the tools: this package imports nothing internal, so the
// run can call load's answer but not the other way round. Lexical only;
// symlinks are the caller's.
//
// POSIX path rather than filepath: pipeline paths are written with "/", a
// container's tree is POSIX whatever runs steps, and steps ships for linux and
// darwin, where the two agree.
func ConfinedPath(dir, rel string) (string, bool) {
	resolved := path.Clean(rel)
	if !path.IsAbs(resolved) {
		resolved = path.Join(dir, rel)
	}

	base := path.Clean(dir)

	switch {
	case resolved == base:
		return resolved, true
	case base == ".":
		return resolved, !path.IsAbs(resolved) && resolved != ".." && !strings.HasPrefix(resolved, "../")
	default:
		return resolved, strings.HasPrefix(resolved, base+"/")
	}
}

// ValidateArtifactPath accepts what a capture may materialize to: an artifact
// name, optionally followed by /-separated segments inside it — a collecting
// matrix captures a cell's output at findings/alpha/fast (see
// CollectedOutputMapping). The first segment is a full artifact name,
// reservations included; later segments need only the charset, since they
// name directories inside an artifact rather than artifacts. Every segment
// matching the pattern is what makes the path safe to join: no segment can be
// empty, "..", or absolute, so it implies ConfinedPath without asking it.
//
// USER-authored mapping values never reach this looser rule — they are pinned
// to ValidateArtifactName at load (see validateArtifactMappings), so the only
// slash-carrying paths are machine-composed from already-validated parts.
func ValidateArtifactPath(artifactPath string) error {
	segments := strings.Split(artifactPath, "/")

	err := ValidateArtifactName(segments[0])
	if err != nil {
		return err
	}

	for _, segment := range segments[1:] {
		if !artifactNamePattern.MatchString(segment) {
			return fmt.Errorf("invalid artifact path %q: segment %q must match %s", artifactPath, segment, artifactNamePattern.String())
		}
	}

	return nil
}
