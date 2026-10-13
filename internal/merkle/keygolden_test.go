package merkle

import (
	"bytes"
	"encoding/json"
	"flag"
	"hash/fnv"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

var update = flag.Bool("update", false, "regenerate testdata/keyfields.golden from the builders as they are now")

const keyGoldenPath = "testdata/keyfields.golden"

// fillKeyed is base with every zero leaf not named in skip set to a value derived from its own path, so each field's value is distinct and the same on every run. The leaves base already sets are kept: they are what makes the builder accept the value at all (a model a provider resolves, a grant naming one form).
func fillKeyed[T any](t *testing.T, base T, skip map[string]string) T {
	t.Helper()

	filled := deepCopy(t, base)
	root := reflect.ValueOf(&filled).Elem()

	var paths []string

	leafPaths(root, "", &paths)

	for _, path := range paths {
		if _, skipped := skip[path]; skipped {
			continue
		}

		if leaf := resolve(root, path); leaf.IsZero() {
			fillTo(leaf, path, 0)
		}
	}

	return filled
}

// maxFillDepth reaches further than perturbing does, so what sits under a nil pointer inside a nil pointer (an mcp: stage's in: tool and its args) is set too.
const maxFillDepth = 6

func fillTo(v reflect.Value, label string, depth int) {
	if depth > maxFillDepth || fillScalar(v, label) {
		return
	}

	//nolint:exhaustive // the scalar kinds are fillScalar's, and a kind neither handles (a chan, a func) is not something a pipeline can write
	switch v.Kind() {
	case reflect.Slice:
		element := reflect.New(v.Type().Elem()).Elem()
		fillTo(element, label+"[0]", depth+1)
		v.Set(reflect.Append(v, element))
	case reflect.Map:
		key, value := reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem()
		fillTo(key, label+".key", depth+1)
		fillTo(value, label+".value", depth+1)

		filled := reflect.MakeMap(v.Type())
		filled.SetMapIndex(key, value)
		v.Set(filled)
	case reflect.Pointer:
		fresh := reflect.New(v.Type().Elem())
		fillTo(fresh.Elem(), label, depth+1)
		v.Set(fresh)
	case reflect.Struct:
		for i := range v.NumField() {
			if field := v.Type().Field(i); field.IsExported() {
				fillTo(v.Field(i), label+"."+field.Name, depth+1)
			}
		}
	}
}

func fillScalar(v reflect.Value, label string) bool {
	h := fnv.New32a()
	_, _ = h.Write([]byte(label))
	n := h.Sum32()%997 + 1

	//nolint:exhaustive // the composite kinds are fillTo's
	switch v.Kind() {
	case reflect.String:
		v.SetString(label)
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(int64(n % 100))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(uint64(n % 100))
	case reflect.Float32, reflect.Float64:
		v.SetFloat(float64(n%100)/100 + 0.005)
	case reflect.Interface:
		v.Set(reflect.ValueOf(label))
	default:
		return false
	}

	return true
}

// The other two key tests both miss the change that costs the most: TestEveryConfigFieldMovesTheKeyOrIsNamedAsNotMeantTo only asks that a change MOVES the key, and TestUnsetFieldsLeaveTheKeyExactlyAsItWas only pins fields left unset. Renaming the map key a SET field folds under ("user" becoming "usr") passes both, and changes the key of every pipeline that sets the field — every one of those caches is silently thrown away. These are the bytes with every keyed field set at once.
func TestSetFieldsKeyExactlyAsTheyDid(t *testing.T) {
	t.Parallel()

	got := map[string]json.RawMessage{}

	for _, probe := range keyProbes() {
		filled := probe.filled(t, probe)
		if !json.Valid([]byte(filled)) {
			t.Fatalf("%s with every keyed field set did not build: %s", probe.name, filled)
		}

		got[probe.name] = json.RawMessage(filled)
	}

	if *update {
		writeKeyGolden(t, got)

		return
	}

	want := readKeyGolden(t)

	for name := range want {
		if _, probed := got[name]; !probed {
			t.Errorf("%s is pinned in %s but no longer probed — regenerate with -update", name, keyGoldenPath)
		}
	}

	for name, filled := range got {
		pinned, ok := want[name]

		switch {
		case !ok:
			t.Errorf("%s has no golden bytes — regenerate with -update", name)
		case pinned != string(filled):
			t.Errorf("%s with every keyed field set:\n got %s\nwant %s\na cache key changed for a pipeline that SETS this field — that invalidates every such cache; if intended, regenerate with -update and say so in the commit", name, filled, pinned)
		}
	}
}

func writeKeyGolden(t *testing.T, got map[string]json.RawMessage) {
	t.Helper()

	raw, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}

	err = os.MkdirAll(filepath.Dir(keyGoldenPath), 0o750)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(keyGoldenPath, append(raw, '\n'), 0o600)
	if err != nil {
		t.Fatal(err)
	}
}

// readKeyGolden is the pinned bytes per probe, compacted back to what the builder marshals.
func readKeyGolden(t *testing.T) map[string]string {
	t.Helper()

	raw, err := os.ReadFile(keyGoldenPath)
	if err != nil {
		t.Fatalf("%v — generate it with go test ./internal/merkle -run TestSetFieldsKeyExactlyAsTheyDid -update", err)
	}

	pinned := map[string]json.RawMessage{}

	err = json.Unmarshal(raw, &pinned)
	if err != nil {
		t.Fatal(err)
	}

	want := make(map[string]string, len(pinned))

	for name, indented := range pinned {
		var compact bytes.Buffer

		err = json.Compact(&compact, indented)
		if err != nil {
			t.Fatal(err)
		}

		want[name] = compact.String()
	}

	return want
}
