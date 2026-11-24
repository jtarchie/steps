package agent

import (
	"os"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/shell"
)

// The token in the mcp-config is the bridge's only credential, so a fenced command of another step reading the file can call this step's tools.
func TestCLIBridgeConfigIsHiddenFromAFencedCommand(t *testing.T) {
	t.Parallel()

	bridge, err := newCLIBridge(t.Context(), bridgeConversation(nil, nil, nil), nil)
	if err != nil {
		t.Fatalf("newCLIBridge: %v", err)
	}

	t.Cleanup(func() { _ = bridge.Close(t.Context()) })

	path, err := bridge.writeConfig()
	if err != nil {
		t.Fatalf("writeConfig: %v", err)
	}

	t.Cleanup(func() { _ = os.Remove(path) })

	runner, err := shell.NewRunner(shell.RunnerSpec{Cwd: t.TempDir(), Sandbox: true})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	t.Cleanup(func() { _ = runner.Close() })

	stdout, stderr, code, err := shell.RunCaptureFull(t.Context(), runner, "cat '"+path+"'")
	if err != nil {
		t.Fatalf("cat %s: %v", path, err)
	}

	if code == 0 || !strings.Contains(stderr, "Operation not permitted") {
		t.Errorf("a fenced command read the bridge config: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}
