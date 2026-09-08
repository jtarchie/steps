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

// EncodeVersion renders a version as the canonical JSON every version table
// keys on. json.Marshal sorts map keys, so the same version always produces
// the same string.
func EncodeVersion(version map[string]any) (string, error) {
	encoded, err := json.Marshal(version)
	if err != nil {
		return "", fmt.Errorf("could not encode version: %w", err)
	}

	return string(encoded), nil
}

// DecodeVersion parses one stored version, keeping numbers as exact digits.
func DecodeVersion(encoded string) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader([]byte(encoded)))
	decoder.UseNumber()

	var version map[string]any

	err := decoder.Decode(&version)
	if err != nil {
		return nil, fmt.Errorf("could not decode version: %w", err)
	}

	return version, nil
}
