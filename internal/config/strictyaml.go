package config

// Strict YAML decoding: unknown keys are load errors, everywhere. yaml.v3's
// KnownFields(true) covers every field the decoder walks itself, and
// rejectUnknownKeys covers the mappings this package decodes by hand.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
)

// strictUnmarshal decodes YAML into out, rejecting any key that has no
// corresponding field. It replaces yaml.Unmarshal at every site that reads a
// user-authored document, so a typo (promt:, on_fail:, ressources:) fails the
// load instead of being silently dropped and changing what the pipeline does.
//
// An empty document decodes to the zero value rather than an error, matching
// what yaml.Unmarshal did: yaml.Decoder reports io.EOF where Unmarshal simply
// left the target untouched.
func strictUnmarshal(data []byte, out any) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	err := dec.Decode(out)
	if err != nil && !errors.Is(err, io.EOF) {
		return err //nolint:wrapcheck // every caller adds its own file/context prefix
	}

	return nil
}

// rejectUnknownKeys fails when node, a mapping, carries a key outside allowed.
//
// It exists because KnownFields does not reach through a custom
// yaml.Unmarshaler: a type with its own UnmarshalYAML decodes its mapping via
// yaml.Node.Decode, which starts a fresh, non-strict decoder. Without this,
// the scalar-or-mapping fields (when:, message_files:, fix:, and every tools:
// entry) would be the one place in a pipeline where a misspelled key is
// silently ignored — precisely the fields whose mapping form a user reaches
// for when they want to be explicit.
func rejectUnknownKeys(node *yaml.Node, context string, allowed ...string) error {
	known := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		known[key] = struct{}{}
	}

	// A mapping node's Content alternates key, value, key, value...
	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode := node.Content[i]

		var key string

		err := keyNode.Decode(&key)
		if err != nil {
			// A non-scalar key. Let the real Decode report it in its own terms.
			continue
		}

		_, ok := known[key]
		if ok {
			continue
		}

		return fmt.Errorf("%s at line %d: unknown key %q%s (valid: %s)",
			context, keyNode.Line, key, suggestion(key, allowed), strings.Join(allowed, ", "))
	}

	return nil
}
