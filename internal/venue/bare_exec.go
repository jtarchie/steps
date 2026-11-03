package venue

import (
	"context"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/jtarchie/steps/internal/shell"
)

func (s *bareSession) run(ctx context.Context, command string, out *shell.Output) (int, error) {
	workdir := s.dir
	if s.spec.Subdir != "" {
		workdir = path.Join(s.dir, s.spec.Subdir)
	}

	// One pid file per command: an agent's concurrent tool calls share this session, and a cancel must signal its own command's group, not whichever started last.
	pidfile := s.dir + ".pid." + randomSuffix()

	// Only execs: sshd made this session's shell a session leader, so the pid it records is also the group cancel signals.
	// The env arrives on stdin, never in argv: a passed-through secret on the command line is readable by every user on the worker for as long as the command runs.
	script := `exec sh -c 'echo $$ > "$1"; eval "$(cat)"; cd "$2" || exit 1; exec sh -c "$3"' sh ` +
		shellQuote(pidfile) + " " + shellQuote(workdir) + " " + shellQuote(command)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	stopped := make(chan struct{})

	go func() {
		defer close(stopped)

		<-runCtx.Done()

		if ctx.Err() != nil {
			s.kill(context.WithoutCancel(ctx), pidfile)
		}
	}()

	code, err := s.exec(runCtx, script, strings.NewReader(s.envScript(ctx)), out.Stdout, out.Stderr)

	cancel()
	<-stopped

	if ctx.Err() != nil {
		return 0, fmt.Errorf("%w", ctx.Err())
	}

	if err != nil {
		s.mu.Lock()
		s.orphans = append(s.orphans, pidfile)
		s.mu.Unlock()
	}

	return code, err
}

// envScript is the command's env as shell exports: the step's, its worker tag, and this command's build metadata.
func (s *bareSession) envScript(ctx context.Context) string {
	env := withWorkerTag(resolveEnv(s.spec.Env), s.spec.WorkerTag)

	var script strings.Builder

	for _, vars := range []map[string]string{env, shell.BuildEnv(ctx)} {
		for name, value := range vars {
			script.WriteString("export " + name + "=" + shellQuote(value) + "\n")
		}
	}

	return script.String()
}

// pidFiles globs every command's pid file beside dir.
func pidFiles(dir string) string { return shellQuote(dir) + ".pid.*" }

// kill ends the command's process group: TERM, then KILL if it is still there after a grace period.
func (s *bareSession) kill(ctx context.Context, pidfile string) {
	pidfile = shellQuote(pidfile)
	signal := func(sig string) {
		_, _ = s.exec(ctx, `p=$(cat `+pidfile+` 2>/dev/null) && { kill -`+sig+` -- -"$p" 2>/dev/null || kill -`+sig+` "$p" 2>/dev/null; }; true`, nil, io.Discard, io.Discard)
	}

	signal("TERM")

	// Polled from here rather than slept on the worker: fractional sleep is not POSIX, and a cancel that always waits the whole grace slows every race: and fail_fast.
	deadline := time.Now().Add(killGrace)

	for time.Now().Before(deadline) && ctx.Err() == nil {
		code, err := s.exec(ctx, `p=$(cat `+pidfile+` 2>/dev/null) && kill -0 -- -"$p" 2>/dev/null`, nil, io.Discard, io.Discard)
		if err != nil || code != 0 {
			return
		}

		time.Sleep(killPoll)
	}

	signal("KILL")
}
