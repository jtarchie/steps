package tree

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHostTreeListDirReportsSizes(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	err := os.WriteFile(filepath.Join(dir, "five.txt"), []byte("12345"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	entries, err := Host{Dir: dir}.ListDir(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 1 || entries[0].Name != "five.txt" || entries[0].Size != 5 {
		t.Errorf("listDir = %+v, want five.txt at 5 bytes", entries)
	}
}

func TestHostTreeListDirIsInNameOrder(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	for _, name := range []string{"c", "a", "b", "e", "d"} {
		err := os.WriteFile(filepath.Join(dir, name), nil, 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	entries, err := Host{Dir: dir}.ListDir(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name)
	}

	if got := strings.Join(names, ""); got != "abcde" {
		t.Errorf("listDir order = %q, want abcde", names)
	}
}
