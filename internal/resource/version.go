package resource

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// errTrailingVersion keeps out:'s refusal of a second value, which the json.Unmarshal it replaced had; a check's array never refused one.
var errTrailingVersion = errors.New("trailing data after version")

// DecodeVersions reads a check's answer, a JSON array of version objects, keeping every number's exact digits.
func DecodeVersions(data []byte) ([]map[string]any, error) {
	var versions []map[string]any

	_, err := decodeExact(data, &versions)
	if err != nil {
		return nil, fmt.Errorf("parsing version array: %w", err)
	}

	return versions, nil
}

// DecodeVersion reads one version object, as out: answers it, keeping every number's exact digits.
func DecodeVersion(data []byte) (map[string]any, error) {
	var version map[string]any

	trailing, err := decodeExact(data, &version)
	if err != nil {
		return nil, fmt.Errorf("parsing version: %w", err)
	}

	if trailing {
		return nil, errTrailingVersion
	}

	return version, nil
}

// decodeExact keeps numbers as json.Number because a version goes straight back out (in:'s template, a put's payload, an API query): float64 turns a snowflake id or Slack's ts into digits no API has heard of. It mirrors store.DecodeVersion, which this package must not import; FuzzVersionReadersAgree in internal/trigger holds the two together.
func decodeExact(data []byte, into any) (bool, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()

	err := decoder.Decode(into)
	if err != nil {
		return false, err //nolint:wrapcheck // every caller names what it was decoding
	}

	return decoder.More(), nil
}
