package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/store/sqlite"
	"github.com/jtarchie/steps/internal/workspace"
)

type writesFail struct {
	store.Store
}

func (writesFail) FinishRun(context.Context, string, store.RunStatus) error {
	return errors.New("finish refused")
}

func (writesFail) ForgetChain(context.Context, string, string) error {
	return errors.New("forget refused")
}

func runOnFailingStore(t *testing.T, run string) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")

	err := os.WriteFile(path, []byte("jobs:\n- name: build\n  plan:\n  - task: work\n    run: "+run+"\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	st, err := sqlite.OpenStore(filepath.Join(dir, "state.db"), "test")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = st.Close() })

	provider, err := workspace.NewProvider(nil, false)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = provider.Close() })

	logged := captureSlog(t)

	_ = captureStdout(t, func() {
		_ = RunJob(context.Background(), cfg, &cfg.Jobs[0], writesFail{st}, Run{Provider: provider})
	})

	return logged.String()
}

// A store write that cannot mask the job's own error is still logged by name, because a lost FinishRun leaves the run 'running' and a lost ForgetChain leaves a skip entry for work that failed.
func TestBestEffortStoreWritesAreLogged(t *testing.T) {
	// Not t.Parallel(): mutates slog's default logger.
	for name, c := range map[string]struct {
		run  string
		want []string
	}{
		"a failed job":  {"exit 1", []string{"operation=FinishRun", "operation=ForgetChain"}},
		"a passing job": {"true", []string{"operation=FinishRun"}},
	} {
		logged := runOnFailingStore(t, c.run)

		for _, want := range c.want {
			if !strings.Contains(logged, want) {
				t.Errorf("%s: log lacks %s:\n%s", name, want, logged)
			}
		}
	}
}
