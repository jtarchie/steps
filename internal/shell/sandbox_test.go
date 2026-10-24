package shell

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type fakeSandbox struct {
	argv []string
	err  error
}

func (f fakeSandbox) command(_ sandboxPolicy, argv []string) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}

	if f.argv != nil {
		return f.argv, nil
	}

	return argv, nil
}

// A fence that will not apply exits nonzero on EVERY command, which the pipeline would read as the step saying no; the probe turns it into the infrastructure error it is, even for a caller that takes exits as data, and Close still removes the private TMPDIR.
func TestAFenceThatCannotApplyIsAnErrorNotAnExit(t *testing.T) {
	for _, tc := range []struct {
		name string
		box  fakeSandbox
		want string
	}{
		{"refused at apply", fakeSandbox{argv: []string{"sh", "-c", "echo sandbox_apply: Operation not permitted >&2; exit 71"}}, "sandbox_apply"},
		{"unsupported here", fakeSandbox{err: errors.New("not on this OS")}, "not on this OS"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)

			fence, err := newHostFence(tc.box, "", true)
			if err != nil {
				t.Fatal(err)
			}

			runner := HostRunner{fence: fence}

			for range 2 {
				_, _, code, err := RunCaptureFull(context.Background(), runner, "true")
				if err == nil || IsExitError(err) || !strings.Contains(err.Error(), tc.want) {
					t.Errorf("RunCaptureFull = exit %d, %v: want an infrastructure error carrying %q on every command", code, err, tc.want)
				}
			}

			err = runner.Close()
			if err != nil {
				t.Fatal(err)
			}

			left, _ := os.ReadDir(tmp)
			if len(left) != 0 {
				t.Errorf("an unusable fence left its private TMPDIR behind: %v", left)
			}
		})
	}
}

func TestDenyReadNamesASymlinkedCredentialDirByItsRealPathToo(t *testing.T) {
	home := t.TempDir()
	target := t.TempDir()

	err := os.Symlink(target, home+"/.ssh")
	if err != nil {
		t.Fatal(err)
	}

	paths := denyReadPaths(home)

	for _, want := range []string{home + "/.ssh", mustResolve(t, target)} {
		if !slices.Contains(paths, want) {
			t.Errorf("deny-read list %v is missing %s", paths, want)
		}
	}
}

func mustResolve(t *testing.T, path string) string {
	t.Helper()

	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}

	return resolved
}
