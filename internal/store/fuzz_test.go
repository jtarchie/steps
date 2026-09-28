package store

import (
	"reflect"
	"testing"
)

func FuzzVersionCanonical(f *testing.F) {
	f.Add(`{"ref":"abc"}`)
	f.Add(`{"b":1,"a":"2"}`)
	f.Add(`{"ts":1699887654.001200,"id":12345678901234567890}`)
	f.Add(`{"nested":{"z":[1,2,{"y":null}]},"u":"é"}`)
	f.Add(`null`)
	f.Add(`{} trailing`)

	f.Fuzz(func(t *testing.T, raw string) {
		version, err := DecodeVersion(raw)
		if err != nil {
			return
		}

		encoded, err := EncodeVersion(version)
		if err != nil {
			t.Fatalf("decoded %q but could not encode it: %v", raw, err)
		}

		again, err := DecodeVersion(encoded)
		if err != nil {
			t.Fatalf("canonical form %q does not decode: %v", encoded, err)
		}

		if !reflect.DeepEqual(version, again) {
			t.Fatalf("round trip changed the version: %#v became %#v", version, again)
		}

		reencoded, err := EncodeVersion(again)
		if err != nil || reencoded != encoded {
			t.Fatalf("canonical form is not a fixed point: %q became %q (%v)", encoded, reencoded, err)
		}
	})
}
