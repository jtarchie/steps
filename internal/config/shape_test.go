package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidShapeExpr(t *testing.T) {
	t.Parallel()

	valid := []string{
		"string", "number", "boolean", "object", "array",
		"array<string>", "array<number>", "array<boolean>", "array<object>", "array<array>",
	}
	for _, expr := range valid {
		err := ValidShapeExpr(expr)
		if err != nil {
			t.Errorf("ValidShapeExpr(%q) = %v, want nil", expr, err)
		}
	}

	invalid := []string{
		"", "String", "int", "map", "stringarray",
		"array<>", "array<int>", "array<array<string>>", "array< string >",
	}
	for _, expr := range invalid {
		err := ValidShapeExpr(expr)
		if err == nil {
			t.Errorf("ValidShapeExpr(%q) = nil, want an error", expr)
		}
	}
}

func TestAssertShapeMismatches(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	write := func(name, body string) {
		err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	write("ids.json", `["a","b"]`)
	write("obj.json", `{"intents":["a"]}`)
	write("nums.json", `[1,2]`)
	write("empty.json", ``)
	write("bad.json", `not json`)

	// Each case is one shape map and the reason substring expected (empty = a pass).
	cases := []struct {
		name  string
		shape map[string]string
		want  string
	}{
		{"array of strings matches", map[string]string{"ids.json": "array<string>"}, ""},
		{"object where array owed", map[string]string{"obj.json": "array<string>"}, "obj.json is not array<string> (got object)"},
		{"array of numbers is not array<string>", map[string]string{"nums.json": "array<string>"}, "nums.json is not array<string> (got array)"},
		{"array of numbers matches array<number>", map[string]string{"nums.json": "array<number>"}, ""},
		{"object matches object", map[string]string{"obj.json": "object"}, ""},
		{"empty file", map[string]string{"empty.json": "array"}, "empty.json is empty"},
		{"missing file", map[string]string{"gone.json": "array"}, "gone.json does not exist"},
		{"not JSON", map[string]string{"bad.json": "array"}, "bad.json is not valid JSON"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := AssertShapeMismatches(tc.shape, dir)

			switch {
			case tc.want == "" && len(got) != 0:
				t.Fatalf("want no mismatch, got %v", got)
			case tc.want != "" && (len(got) != 1 || got[0] != tc.want):
				t.Fatalf("want [%q], got %v", tc.want, got)
			}
		})
	}
}

func TestAssertShapeReject(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		pipeline string
		want     string
	}{
		{
			name: "unknown shape kind",
			pipeline: `
jobs:
- name: build
  plan:
  - task: work
    inputs: []
    outputs: [out]
    run: "true"
    assert:
      shape:
        out/f.json: bogus
`,
			want: `shape "bogus" is not a known kind`,
		},
		{
			name: "path names no declared output",
			pipeline: `
jobs:
- name: build
  plan:
  - task: work
    inputs: []
    outputs: [out]
    run: "true"
    assert:
      shape:
        nope/f.json: array<string>
`,
			want: "which is not one of this step's outputs",
		},
		{
			name: "path is the whole output directory",
			pipeline: `
jobs:
- name: build
  plan:
  - task: work
    inputs: []
    outputs: [out]
    run: "true"
    assert:
      shape:
        out: array
`,
			want: "which is a directory",
		},
		{
			name: "shape on a pipeline assert",
			pipeline: `
assert:
  shape:
    out/f.json: array

jobs:
- name: build
  plan:
  - task: work
    inputs: []
    outputs: [out]
    run: "true"
`,
			want: "shape/nudge are only valid on task/agent step asserts",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			path := writeConfig(t, tt.pipeline)
			wantLoadError(t, path, tt.want)
		})
	}
}

func TestAssertShapeAccepts(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
jobs:
- name: build
  plan:
  - task: work
    inputs: []
    outputs: [out]
    run: "true"
    assert:
      shape:
        out/index.json: array<string>
`)

	_, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("a valid assert.shape should load, got: %v", err)
	}
}
