package tree

import "testing"

func TestStagingPathIsUniquePerWrite(t *testing.T) {
	t.Parallel()

	first, second := stagingPath("/work/a.txt"), stagingPath("/work/b.txt")
	if first == second {
		t.Errorf("two writes into /work share staging file %q", first)
	}
}
