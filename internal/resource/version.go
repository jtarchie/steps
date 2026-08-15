package resource

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// ParseVersionJSON decodes a stored version object — the JSON steps itself
// wrote into resource_checks.version_json — back into the map shape a check
// template renders against.
//
// It decodes with UseNumber so a numeric field survives as its exact digits
// rather than a float64. That matters because these values go straight into a
// template and out over the wire: Slack's ts (1699887654.001200), a snowflake
// id, a build number. encoding/json's default float64 would render the first
// as 1.6998876540012e+09 and hand the API something it has never seen. The
// mcp path already makes the same promise with exactNumbers.
//
// It lives here rather than in the store because both callers that hold a
// store (internal/trigger, internal/pipeline) must agree on it exactly — and
// because this package, which owns what a version IS, cannot import that one.
func ParseVersionJSON(versionJSON string) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader([]byte(versionJSON)))
	decoder.UseNumber()

	var version map[string]any

	err := decoder.Decode(&version)
	if err != nil {
		return nil, fmt.Errorf("parsing recorded version: %w", err)
	}

	return version, nil
}
