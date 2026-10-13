package merkle

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// normalizeMapKeys rewrites every map[any]any nested in value into a
// map[string]any, so the result is marshalable by encoding/json. It mirrors
// resource.renderArgValue's handling of the same shape; the two must agree,
// because one decides what the call sends and the other what its cache key
// is.
func normalizeMapKeys(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, inner := range typed {
			out[key] = normalizeMapKeys(inner)
		}

		return out
	case map[any]any:
		out := make(map[string]any, len(typed))
		for key, inner := range typed {
			out[fmt.Sprint(key)] = normalizeMapKeys(inner)
		}

		return out
	case []any:
		out := make([]any, len(typed))
		for i, inner := range typed {
			out[i] = normalizeMapKeys(inner)
		}

		return out
	default:
		return value
	}
}

// HashNode computes a Node's content-addressed hash: sha256 hex of the
// canonical JSON of {kind, content, parent}. encoding/json.Marshal sorts
// map keys, which is what makes this deterministic — content must stay
// built from plain maps/slices/scalars for that guarantee to hold.
func HashNode(kind NodeKind, content map[string]any, parentHash string) (string, error) {
	payload := map[string]any{
		"kind":    string(kind),
		"content": content,
		"parent":  parentHash,
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("hash %s node: %w", kind, err)
	}

	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:]), nil
}
