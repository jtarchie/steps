package resource

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadIssueTitle(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	for name, body := range map[string]string{"one.txt": "Fix login\r\n\n", "blank.txt": " \n", "two.txt": "Fix login\nthen search\n", "cr.txt": "Fix login\rthen search"} {
		err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	title, err := readIssueTitle(dir, "one.txt")
	if err != nil || title != "Fix login" {
		t.Errorf("one.txt = %q, %v; want the line without its newline", title, err)
	}

	for name, tc := range map[string]struct{ path, want string }{
		"empty":     {"blank.txt", "is empty"},
		"two lines": {"two.txt", "is 2 lines"},
		"a bare cr": {"cr.txt", "a title is one line"},
		"missing":   {"none.txt", "params.title_file"},
		"outside":   {"../../etc/passwd", "params.title_file"},
	} {
		_, err := readIssueTitle(dir, tc.path)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}
