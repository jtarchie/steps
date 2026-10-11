package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// /dev/zero is a character device that is not the null device, the nearest stand-in for a tty a test can open.
func TestIsTerminalWantsACharacterDevice(t *testing.T) {
	t.Parallel()

	device, err := os.Open("/dev/zero")
	if err != nil {
		t.Skipf("no /dev/zero to stand in for a terminal: %v", err)
	}

	defer func() { _ = device.Close() }()

	if !isTerminal(device) {
		t.Error("a character device other than /dev/null was refused as a terminal")
	}

	file, err := os.Create(filepath.Join(t.TempDir(), "stdin"))
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = file.Close() }()

	if isTerminal(file) {
		t.Error("a regular file was taken for a terminal")
	}
}
