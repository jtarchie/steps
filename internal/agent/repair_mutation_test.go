package agent

import (
	"io"
	"strings"
	"testing"
)

func TestRepairJSONArgsEdges(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want string
	}{
		{in: `{`, want: `{}`},
		{in: `{"a":1, }`, want: `{"a":1 }`},
		{in: `{"a":[1, 2 ,  ]}`, want: `{"a":[1, 2   ]}`},
	}

	for _, test := range tests {
		got, changed := repairJSONArgs(test.in)
		if !changed || got != test.want {
			t.Errorf("repairJSONArgs(%q) = %q, %v; want %q, true", test.in, got, changed, test.want)
		}
	}
}

func TestReadBodyCappedLimitIsInclusive(t *testing.T) {
	t.Parallel()

	const limit = 8

	body, err := readBodyCapped(io.NopCloser(strings.NewReader(strings.Repeat("a", limit))), limit)
	if err != nil || len(body) != limit {
		t.Errorf("a body of exactly the limit: %d bytes, err %v; want all of it and no error", len(body), err)
	}

	body, err = readBodyCapped(io.NopCloser(strings.NewReader(strings.Repeat("a", limit+1))), limit)
	if err == nil || len(body) != limit {
		t.Errorf("a body one over the limit: %d bytes, err %v; want the first %d and an error", len(body), err, limit)
	}
}
