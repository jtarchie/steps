package resource

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func FuzzVersionDecoders(f *testing.F) {
	f.Add(`{"ref":"abc"}`)
	f.Add(`{"ts":1699887654.001200}`)
	f.Add(`{"a":1} {"b":2}`)
	f.Add(`[{"ref":"a"},{"ref":"b"}]`)
	f.Add(`[{"ref":"a"}] trailing`)
	f.Add(`null`)

	f.Fuzz(func(t *testing.T, raw string) {
		parsed, parseErr := ParseVersionJSON(raw)

		out, outErr := decodeOutVersion([]byte(raw))
		if outErr == nil {
			if parseErr != nil {
				t.Fatalf("out: accepted %q that a recorded version refuses: %v", raw, parseErr)
			}

			if !reflect.DeepEqual(out, parsed) {
				t.Fatalf("out: and recorded decoders disagree on %q: %#v vs %#v", raw, out, parsed)
			}
		}

		if parseErr == nil && parsed != nil {
			checkStored(t, parsed)
		}

		checked, checkErr := decodeVersionArray([]byte(raw))
		mcp, mcpErr := decodeMapSlice([]byte(raw))

		if (checkErr == nil) != (mcpErr == nil) || !reflect.DeepEqual(checked, mcp) {
			t.Fatalf("shell and mcp checks disagree on %q: %#v (%v) vs %#v (%v)", raw, checked, checkErr, mcp, mcpErr)
		}

		for _, version := range checked {
			if version != nil {
				checkStored(t, version)
			}
		}
	})
}

// checkStored holds a version to surviving the trip into resource_checks.version_json and back, digits intact.
func checkStored(t *testing.T, version map[string]any) {
	t.Helper()

	encoded, err := json.Marshal(version)
	if err != nil {
		t.Fatalf("version %#v does not encode: %v", version, err)
	}

	stored, err := ParseVersionJSON(string(encoded))
	if err != nil || !reflect.DeepEqual(stored, version) {
		t.Fatalf("%q does not read back as the version it was: %#v vs %#v (%v)", encoded, stored, version, err)
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
