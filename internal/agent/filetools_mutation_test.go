package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestExecListDirListsTheNamedPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	err := os.MkdirAll(filepath.Join(dir, "sub"), 0o750)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(filepath.Join(dir, "sub", "inner.txt"), nil, 0o600)
	if err != nil {
		t.Fatal(err)
	}

	result := execListDir(context.Background(), map[string]any{"path": "sub"}, testEnv(dir))

	entries, ok := result["entries"].([]map[string]any)
	if !ok || len(entries) != 1 || entries[0]["name"] != "inner.txt" {
		t.Errorf("entries = %v, want sub's own inner.txt", result["entries"])
	}
}

func TestExecListDirAtExactlyTheCap(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	for i := range maxListDirEntries {
		err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("file-%05d.txt", i)), nil, 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	result := execListDir(context.Background(), map[string]any{}, testEnv(dir))

	if result["truncated"] != false || result["total"] != maxListDirEntries {
		t.Errorf("truncated = %v, total = %v; want a full listing of exactly %d", result["truncated"], result["total"], maxListDirEntries)
	}
}
