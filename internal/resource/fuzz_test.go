package resource

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func FuzzDecodeVersions(f *testing.F) {
	f.Add(`{"ref":"abc"}`)
	f.Add(`{"ts":1699887654.001200}`)
	f.Add(`{"id":12345678901234567890}`)
	f.Add(`{"a":1} {"b":2}`)
	f.Add(`{"a":1}}`)
	f.Add(`[{"ref":"a"},{"ref":"b"}]`)
	f.Add(`[{"ref":"a"}] trailing`)
	f.Add(`[{"a":{"b":[1,2.50,null]}},null]`)
	f.Add(`null`)

	f.Fuzz(func(t *testing.T, raw string) {
		out, err := DecodeVersion([]byte(raw))
		if err == nil && out != nil {
			checkStable(t, out)
		}

		checked, err := DecodeVersions([]byte(raw))
		if err != nil {
			return
		}

		for _, version := range checked {
			if version != nil {
				checkStable(t, version)
			}
		}
	})
}

// checkStable holds a version to surviving encode and decode with its digits and its encoding unchanged, since the encoding is the key a version is filed under.
func checkStable(t *testing.T, version map[string]any) {
	t.Helper()

	encoded, err := json.Marshal(version)
	if err != nil {
		t.Fatalf("version %#v does not encode: %v", version, err)
	}

	again, err := DecodeVersion(encoded)
	if err != nil || !reflect.DeepEqual(again, version) {
		t.Fatalf("%q does not read back as the version it was: %#v vs %#v (%v)", encoded, again, version, err)
	}

	reencoded, err := json.Marshal(again)
	if err != nil || !bytes.Equal(reencoded, encoded) {
		t.Fatalf("%q re-encodes differently: %q (%v)", encoded, reencoded, err)
	}
}

func FuzzReadParamFile(f *testing.F) {
	root := f.TempDir()
	srcDir := filepath.Join(root, "src")

	for _, dir := range []string{filepath.Join(srcDir, "answer"), filepath.Join(root, "secret")} {
		err := os.MkdirAll(dir, 0o750)
		if err != nil {
			f.Fatal(err)
		}
	}

	for name, content := range map[string]string{
		filepath.Join(srcDir, "answer", "reply.md"): "inside",
		filepath.Join(root, "outside.md"):           "outside",
		filepath.Join(root, "secret", "key"):        "outside",
	} {
		err := os.WriteFile(name, []byte(content), 0o600)
		if err != nil {
			f.Fatal(err)
		}
	}

	for link, target := range map[string]string{"escape": filepath.Join(root, "outside.md"), "up": "../../secret"} {
		err := os.Symlink(target, filepath.Join(srcDir, "answer", link))
		if err != nil {
			f.Fatal(err)
		}
	}

	f.Add("answer/escape")
	f.Add("answer/up/key")
	f.Add("answer/reply.md")
	f.Add("../outside.md")
	f.Add("answer/../../secret/key")
	f.Add("./answer//reply.md")
	f.Add("/etc/passwd")
	f.Add("..\\outside.md")

	f.Fuzz(func(t *testing.T, declared string) {
		got, err := readParamFile(declared, srcDir)
		if err != nil {
			return
		}

		if strings.Contains(got, "outside") {
			t.Fatalf("%q read a file outside the put's inputs", declared)
		}
	})
}
