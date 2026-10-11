package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAContextFileOfExactlyTheLimitIsWhole(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte("12345678"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	blocks, err := loadContextBlocks(t.Context(), dir, []string{"notes.md"}, 8, imageSight{})
	if err != nil {
		t.Fatal(err)
	}

	if len(blocks) != 1 || blocks[0].content != "12345678" {
		t.Errorf("got %+v; want the file whole with no truncation notice", blocks)
	}
}
