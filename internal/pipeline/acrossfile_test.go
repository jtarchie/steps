package pipeline

// Decoding an across: axis's file. The array is produced during the run, often
// by a model, so every shape that is not a JSON array of strings has to fail
// naming the file rather than expanding to something surprising.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

func TestDecodeAcrossItems(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		data    string
		want    []string
		wantErr string
	}{
		{name: "array of strings", data: `["alpha","beta"]`, want: []string{"alpha", "beta"}},
		{name: "empty array is legal", data: `[]`, want: []string{}},
		{name: "whitespace and newlines", data: "[\n  \"alpha\"\n]\n", want: []string{"alpha"}},
		{
			name:    "an object is not a list",
			data:    `{"items":["alpha"]}`,
			wantErr: "must hold a JSON array of strings",
		},
		{
			name:    "objects are not strings",
			data:    `[{"id":"alpha"}]`,
			wantErr: "must hold a JSON array of strings",
		},
		{
			name:    "numbers are not strings",
			data:    `[1,2]`,
			wantErr: "must hold a JSON array of strings",
		},
		{
			name:    "not JSON at all",
			data:    "alpha\nbeta\n",
			wantErr: "must hold a JSON array of strings",
		},
		{
			name:    "empty file",
			data:    "",
			wantErr: "must hold a JSON array of strings",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := decodeAcrossItems("findings/items.json", []byte(tc.data))

			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("decodeAcrossItems: no error, want one containing %q", tc.wantErr)
				}

				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
				}

				// Every message names the file, since the author's next move is
				// to look at what wrote it.
				if !strings.Contains(err.Error(), "findings/items.json") {
					t.Errorf("error = %v, want it to name the file", err)
				}

				return
			}

			if err != nil {
				t.Fatalf("decodeAcrossItems: %v", err)
			}

			if len(got) != len(tc.want) {
				t.Fatalf("items = %v, want %v", got, tc.want)
			}

			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("item %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestDecodeAcrossItemsCapsTheList pins the ceiling. The array is produced
// during the run, so its length was never reviewed: an unbounded one turns an
// upstream typo into an unbounded bill, and the message has to say what to do.
func TestDecodeAcrossItemsCapsTheList(t *testing.T) {
	t.Parallel()

	items := make([]string, 0, config.MaxAcrossItems+1)
	for i := range config.MaxAcrossItems + 1 {
		items = append(items, fmt.Sprintf("%q", fmt.Sprintf("item-%d", i)))
	}

	data := "[" + strings.Join(items, ",") + "]"

	_, err := decodeAcrossItems("findings/items.json", []byte(data))
	if err == nil || !strings.Contains(err.Error(), "above the limit") {
		t.Fatalf("error = %v, want the item cap to refuse the list", err)
	}
}
