package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/testdocker"
)

// landlockPipeline states the linux fence as exit codes, so a run that passes is the sandbox holding on every point.
const landlockPipeline = `
jobs:
- name: build
  plan:
  - task: probe
    sandbox: true
    run: |
      touch inside || exit 10
      if touch /home/ubuntu/escaped 2>/dev/null; then exit 11; fi
      if cat /home/ubuntu/.ssh/id_ed25519 2>/dev/null; then exit 12; fi
      cat /etc/os-release >/dev/null || exit 13
      touch "$TMPDIR/scratch" || exit 14
      ls /home/ubuntu >/dev/null || exit 15
      echo fenced
`

// The Landlock fence holds only in a real linux process tree, which an in-process run on a darwin test machine cannot be, so this crosses the whole seam: a linux steps binary, a non-root user in a stock ubuntu:24.04 under docker's default seccomp, and `steps run` re-executing itself as the sandbox helper.
func TestLandlockSandboxFencesAHostTaskOnLinux(t *testing.T) {
	t.Parallel()

	testdocker.Require(t)
	dockerSlot(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	box := startLandlockBox(ctx, t)

	box.put(ctx, t, "fenced.yml", landlockPipeline)
	box.put(ctx, t, "loose.yml", strings.Replace(landlockPipeline, "    sandbox: true\n", "", 1))
	box.put(ctx, t, "offline.yml", strings.Replace(landlockPipeline, "    sandbox: true\n", "    sandbox: true\n    network: none\n", 1))
	box.put(ctx, t, "steps.yml", "web:\n  fence: warn\n")

	out, err := box.steps(ctx, "run", "fenced.yml", "--job", "build")
	if err != nil || !strings.Contains(out, "fenced") {
		t.Fatalf("sandboxed task failed (exit 10: workspace not writable, 11: wrote outside, 12: read ~/.ssh, 13: /etc unreadable, 14: no TMPDIR, 15: ~ not listable): %v\n%s", err, out)
	}

	if box.exec(ctx, "test", "-e", "/home/ubuntu/escaped") == nil {
		t.Fatal("the fenced task's write outside its workspace landed")
	}

	// The control: the same task unfenced does write outside, so exit 11 above is the fence and not the container.
	out, err = box.steps(ctx, "run", "loose.yml", "--job", "build")
	if err == nil || !strings.Contains(out, "11") {
		t.Errorf("the unfenced control did not escape, so the test proves nothing: %v\n%s", err, out)
	}

	out, err = box.steps(ctx, "run", "offline.yml", "--job", "build")
	if err == nil || !strings.Contains(out, "network: none is not available on linux") {
		t.Errorf("network: none under a linux sandbox ran rather than refusing by name: %v\n%s", err, out)
	}

	out, err = box.steps(ctx, "validate", "--config", "steps.yml", "fenced.yml")
	if err != nil || strings.Contains(out, `task "probe"`) {
		t.Errorf("fence: warn on a Landlock machine listed the sandboxed task as unfenced: %v\n%s", err, out)
	}
}

// A kernel without Landlock is played by a seccomp profile answering its syscalls ENOSYS, as an old kernel does: the step refuses by name rather than running unfenced, and fence: warn lists it, because the fence is judged by the machine and not the OS name.
func TestLandlockSandboxRefusesWhereTheKernelHasNone(t *testing.T) {
	t.Parallel()

	testdocker.Require(t)
	dockerSlot(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	profile := filepath.Join(t.TempDir(), "seccomp.json")

	err := os.WriteFile(profile, []byte(`{"defaultAction": "SCMP_ACT_ALLOW", "syscalls": [{"names": ["landlock_create_ruleset", "landlock_add_rule", "landlock_restrict_self"], "action": "SCMP_ACT_ERRNO", "errnoRet": 38}]}`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	box := startLandlockBox(ctx, t, "--security-opt", "seccomp="+profile)

	box.put(ctx, t, "fenced.yml", landlockPipeline)
	box.put(ctx, t, "steps.yml", "web:\n  fence: warn\n")

	out, err := box.steps(ctx, "run", "fenced.yml", "--job", "build")
	if err == nil || !strings.Contains(out, "Landlock is not enabled") {
		t.Errorf("a sandboxed task on a kernel without Landlock did not refuse by name: %v\n%s", err, out)
	}

	if box.exec(ctx, "test", "-e", "/home/ubuntu/escaped") == nil {
		t.Error("the refused task ran unfenced")
	}

	out, err = box.steps(ctx, "validate", "--config", "steps.yml", "fenced.yml")
	if err != nil || !strings.Contains(out, `task "probe"`) {
		t.Errorf("fence: warn on a machine without Landlock did not list the sandboxed task: %v\n%s", err, out)
	}
}

// landlockBox is an ubuntu:24.04 container holding a linux steps binary, a planted ~/.ssh key, and a work dir owned by its non-root ubuntu user.
type landlockBox struct{ name string }

func startLandlockBox(ctx context.Context, t *testing.T, runFlags ...string) landlockBox {
	t.Helper()

	binary := filepath.Join(t.TempDir(), "steps")

	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "github.com/jtarchie/steps") //nolint:gosec // a temp path this test made
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+runtime.GOARCH, "CGO_ENABLED=0")

	out, err := build.CombinedOutput()
	if err != nil {
		t.Fatalf("cross-compile steps: %v\n%s", err, out)
	}

	box := landlockBox{name: fmt.Sprintf("steps-landlock-%d-%d", os.Getpid(), time.Now().UnixNano())}

	box.must(ctx, t, "", append(append([]string{"run", "-d", "--name", box.name}, runFlags...), "ubuntu:24.04", "sleep", "infinity")...)
	t.Cleanup(func() { _ = box.docker(context.WithoutCancel(ctx), "", "rm", "-f", box.name).Run() })

	box.must(ctx, t, "", "cp", binary, box.name+":/usr/local/bin/steps")
	box.must(ctx, t, "", "exec", box.name, "sh", "-c", `mkdir -p /home/ubuntu/.ssh /home/ubuntu/work && echo secret > /home/ubuntu/.ssh/id_ed25519 && chown -R ubuntu /home/ubuntu`)

	return box
}

func (b landlockBox) docker(ctx context.Context, stdin string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "docker", args...) //nolint:gosec // this test's own argv
	cmd.Stdin = strings.NewReader(stdin)

	return cmd
}

func (b landlockBox) must(ctx context.Context, t *testing.T, stdin string, args ...string) {
	t.Helper()

	out, err := b.docker(ctx, stdin, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func (b landlockBox) put(ctx context.Context, t *testing.T, file, body string) {
	t.Helper()

	b.must(ctx, t, body, "exec", "-i", "-u", "ubuntu", b.name, "sh", "-c", "cat > /home/ubuntu/work/"+file)
}

func (b landlockBox) exec(ctx context.Context, args ...string) error {
	return b.docker(ctx, "", append([]string{"exec", b.name}, args...)...).Run() //nolint:wrapcheck // the caller reads only whether it exited zero
}

func (b landlockBox) steps(ctx context.Context, args ...string) (string, error) {
	argv := append([]string{"exec", "-u", "ubuntu", "-w", "/home/ubuntu/work", "-e", "HOME=/home/ubuntu", b.name, "steps"}, args...)
	out, err := b.docker(ctx, "", argv...).CombinedOutput()

	return string(out), err
}
