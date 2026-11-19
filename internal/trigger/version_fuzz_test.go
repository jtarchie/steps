package trigger

import (
	"reflect"
	"testing"

	rsrc "github.com/jtarchie/steps/internal/resource"
	"github.com/jtarchie/steps/internal/store"
)

// FuzzVersionReadersAgree lives here because resource may not import store: this package hands what a check or out: reported to the store and reads it back, so the two decoders must agree on every version.
func FuzzVersionReadersAgree(f *testing.F) {
	f.Add(`{"ref":"abc"}`)
	f.Add(`{"ts":1699887654.001200}`)
	f.Add(`{"id":12345678901234567890}`)
	f.Add(`{"a":1} {"b":2}`)
	f.Add(`{"a":1}}`)
	f.Add(`[{"ref":"a"},{"ts":1699887654.001200}]`)
	f.Add(`[{"a":{"b":[1,2.50,null]}},null]`)
	f.Add(`null`)

	f.Fuzz(func(t *testing.T, raw string) {
		out, outErr := rsrc.DecodeVersion([]byte(raw))
		if outErr == nil {
			stored, storedErr := store.DecodeVersion(store.VersionKey(raw))
			if storedErr != nil || !reflect.DeepEqual(out, stored) {
				t.Fatalf("out: and recorded readers disagree on %q: %#v vs %#v (%v)", raw, out, stored, storedErr)
			}
		}

		checked, err := rsrc.DecodeVersions([]byte(raw))
		if err != nil {
			return
		}

		for _, version := range checked {
			encoded, err := store.EncodeVersion(version)
			if err != nil {
				t.Fatalf("version %#v does not encode: %v", version, err)
			}

			stored, err := store.DecodeVersion(encoded)
			if err != nil || !reflect.DeepEqual(stored, version) {
				t.Fatalf("%q does not read back from the store as the version a check reported: %#v vs %#v (%v)", encoded, stored, version, err)
			}
		}
	})
}
