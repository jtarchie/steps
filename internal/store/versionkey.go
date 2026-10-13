package store

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// DefaultResourceVersionCap bounds resource_versions per resource when
// nothing says otherwise.
//
// A count rather than an age, for the same reason every other cap here is:
// what matters is how far behind a job may fall, not how old a version is. A
// resource nobody has polled in a month has not accumulated anything, while a
// busy one accumulates a row per version forever.
//
// The bound is not free of meaning. A version pruned here takes its
// job_versions row with it (ON DELETE CASCADE), so a `passed:` gate can no
// longer clear for it — correct, since a version out of history cannot be
// built, but it means a cap set below what a slow downstream job needs will
// hold that job back. Override with defaults.version_history: in the
// pipeline, or --version-history on the command line.
const DefaultResourceVersionCap = 1000

// VersionKey is a version as the canonical JSON every version table keys on.
//
// A type rather than a string because two spellings of one version are two
// keys: a version marshalled some other way reaches a table as a version
// nobody recorded, and a passed: gate waits on it forever. EncodeVersion is
// the only constructor outside a driver; a driver hands back what it stored.
type VersionKey string

// EncodeVersion renders a version as its VersionKey. json.Marshal sorts map
// keys, so the same version always produces the same key.
func EncodeVersion(version map[string]any) (VersionKey, error) {
	encoded, err := json.Marshal(version)
	if err != nil {
		return "", fmt.Errorf("could not encode version: %w", err)
	}

	return VersionKey(encoded), nil
}

// DecodeVersion parses one stored version, keeping numbers as exact digits.
func DecodeVersion(encoded VersionKey) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader([]byte(encoded)))
	decoder.UseNumber()

	var version map[string]any

	err := decoder.Decode(&version)
	if err != nil {
		return nil, fmt.Errorf("could not decode version: %w", err)
	}

	return version, nil
}

// DecodeVersions decodes a stored check history in order — what
// ResourceVersionsJSON hands back, for the caller that inspects fields.
func DecodeVersions(encoded []VersionKey) ([]map[string]any, error) {
	versions := make([]map[string]any, 0, len(encoded))

	for _, one := range encoded {
		version, err := DecodeVersion(one)
		if err != nil {
			return nil, err
		}

		versions = append(versions, version)
	}

	return versions, nil
}
