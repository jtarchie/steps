package venue

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jtarchie/steps/internal/testsshd"
)

type testSSHD = testsshd.Server

func newTestSSHD(t *testing.T) *testSSHD { return testsshd.New(t) }

var generateKey = testsshd.GenerateKey

// uploadsUnder counts pushed shim binaries on disk, since sftp.Server reports no writes of its own.
func uploadsUnder(t *testing.T, root string) int {
	t.Helper()

	count := 0

	err := filepath.WalkDir(root, func(_ string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if !entry.IsDir() && entry.Name() == "steps" {
			count++
		}

		return nil
	})
	if err != nil {
		t.Fatalf("counting pushed binaries: %v", err)
	}

	return count
}
