package venue

// The ssh:// venue after steps#206: the worker needs sshd, a POSIX shell and tar, and nothing else. The step's tree goes over as a tar stream, the command runs through the remote shell, the outputs come back as a tar stream. No binary is pushed and nothing is cached on the worker.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"github.com/jtarchie/steps/internal/redact"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/wire"
)

const (
	// killGrace is how long a cancelled command gets between TERM and KILL.
	killGrace = 2 * time.Second
	killPoll  = 100 * time.Millisecond
)

var (
	errImageOnSSH   = errors.New("an ssh:// worker runs steps bare and cannot run image:; map the tag to docker+ssh:// for containers")
	errBareClosed   = errors.New("the worker session is closed")
	errBareProbe    = errors.New("the worker did not describe itself")
	errBareTransfer = errors.New("the worker's tar failed")
)

type bareRunner struct {
	label string
	s     *bareSession
}

type bareSession struct {
	worker Worker
	spec   shell.RunnerSpec

	mu        sync.Mutex
	attempted bool
	startErr  error
	closed    bool

	conn liveSSH
	// orphans are the pid files of commands whose connection failed under them: still running, for all this end knows.
	orphans        []string
	dir            string
	pipeline       string
	goos, goarch   string
	uid, gid       *int
	fetchMu        sync.Mutex
	sent, received atomic.Int64
}

func newBareRunner(worker Worker, spec shell.RunnerSpec) (shell.Runner, error) {
	if spec.Image != "" {
		return nil, fmt.Errorf("%w %q: %w", ErrWorker, worker.URL, errImageOnSSH)
	}

	return bareRunner{s: &bareSession{worker: worker, spec: spec}}, nil
}

// Exec runs cmd on the worker. Stdin is not honoured: the session's stdin carries the step's env.
func (r bareRunner) Exec(ctx context.Context, cmd shell.Command) (shell.Result, error) {
	res, err := r.exchange(ctx, cmd)

	// Under ExitAsData an error means the command never ran: a guard that could not run is not a guard that said no.
	if cmd.ExitAsData {
		if err != nil {
			return shell.Result{Code: -1}, fmt.Errorf("command %q failed to start: %w", redact.Command(cmd.Script), shell.WrapIfCanceled(ctx, err))
		}

		return res, nil
	}

	if err != nil {
		return res, fmt.Errorf("command %q failed: %w", redact.Command(cmd.Script), shell.WrapIfCanceled(ctx, err))
	}

	if res.Code != 0 {
		return res, fmt.Errorf("command %q failed: %w", redact.Command(cmd.Script), &shell.ExitError{Command: cmd.Script, Venue: r.s.worker.String(), Code: res.Code})
	}

	return res, nil
}

func (r bareRunner) WithLabel(label string) shell.Runner {
	r.label = label

	return r
}

func (r bareRunner) Close() error { return r.s.close() }

func (r bareRunner) placement() (Placement, bool) { return r.s.placement() }

// exchange runs one command, then brings the outputs home even after a nonzero exit: an assert: reads the local tree the moment this returns.
func (r bareRunner) exchange(ctx context.Context, cmd shell.Command) (shell.Result, error) {
	err := r.s.ensure(ctx)
	if err != nil {
		return shell.Result{}, err
	}

	out := cmd.Output(ctx, r.label)

	code, err := r.s.run(ctx, cmd.Script, out)

	out.Flush()

	if err != nil {
		return out.Result(0), err
	}

	err = r.s.fetch(ctx)
	if err != nil {
		return out.Result(0), err
	}

	return out.Result(code), nil
}

func (s *bareSession) ensure(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return fmt.Errorf("%w %q: %w", ErrWorker, s.worker.URL, errBareClosed)
	}

	if !s.attempted {
		s.attempted = true

		err := s.connect(ctx)
		if err != nil {
			s.startErr = fmt.Errorf("%w %q: %w", ErrWorker, s.worker.URL, err)
		}

		return s.startErr
	}

	if s.startErr != nil {
		return s.startErr
	}

	_, err := s.conn.redial(ctx, s.dial)
	if err != nil {
		return fmt.Errorf("%w %q: dialling again after the connection dropped: %w", ErrWorker, s.worker.URL, err)
	}

	// Before this command, not at close: a retry run beside the command it replaces would race it in the same directory.
	for _, pidfile := range s.orphans {
		s.kill(ctx, pidfile)
	}

	s.orphans = nil

	return nil
}

func (s *bareSession) dial(ctx context.Context) (*ssh.Client, error) {
	return sshClientFor(ctx, s.worker)
}

func (s *bareSession) connect(ctx context.Context) error {
	client, err := s.dial(ctx)
	if err != nil {
		return err
	}

	s.conn.adopt(client)

	// Once per process per worker: what a dead process left does not appear mid-run, and this is an extra round trip per step.
	if _, done := swept.LoadOrStore("ssh "+s.worker.Address(), true); !done {
		s.sweep(ctx)
	}

	err = s.probe(ctx)
	if err != nil {
		return err
	}

	err = s.upload(ctx)
	if err != nil {
		return err
	}

	return s.attachCaches(ctx)
}

// cachePairs is each cache's kept directory beside the step's place for it, shell-quoted, in a stable order.
func (s *bareSession) cachePairs() [][2]string {
	rels := slices.Sorted(maps.Keys(s.spec.Caches))
	pairs := make([][2]string, 0, len(rels))

	for _, rel := range rels {
		pairs = append(pairs, [2]string{s.root() + "/steps-task-caches/" + s.spec.Caches[rel], shellQuote(path.Join(s.dir, rel))})
	}

	return pairs
}

// attachCaches moves each kept cache over the uploaded stand-in, the rename the local workspace does, on the worker.
func (s *bareSession) attachCaches(ctx context.Context) error {
	s.pipeline = shell.MetadataFrom(ctx).PipelineName

	var script strings.Builder

	script.WriteString(reapScript(ctx, s.root()))

	for _, pair := range s.cachePairs() {
		fmt.Fprintf(&script, `rm -rf %[2]s; mkdir -p "$(dirname %[2]s)"; if [ -d %[1]s ]; then mv %[1]s %[2]s; else mkdir -p %[2]s; fi; `, pair[0], pair[1])
	}

	if script.Len() == 0 {
		return nil
	}

	code, err := s.exec(ctx, script.String(), nil, io.Discard, io.Discard)
	if err == nil && code != 0 {
		err = fmt.Errorf("%w: exit %d", errBareTransfer, code)
	}

	if err != nil {
		return fmt.Errorf("restoring the step's caches: %w", err)
	}

	return nil
}

// detachCaches is the shell that files each cache back, replacing one a concurrent session filed first; run after the step's processes are gone.
func (s *bareSession) detachCaches() string {
	if len(s.spec.Caches) == 0 {
		return ""
	}

	var script strings.Builder

	script.WriteString("mkdir -p " + s.root() + "/steps-task-caches; ")

	for _, pair := range s.cachePairs() {
		// The .pipeline file is what a later run of this pipeline judges the cache by when its config stops naming it.
		fmt.Fprintf(&script, `if [ -d %[2]s ]; then rm -rf %[1]s; mv %[2]s %[1]s && printf %%s %[3]s > %[1]s.pipeline; fi; `, pair[0], pair[1], shellQuote(s.pipeline))
	}

	return script.String()
}

func (s *bareSession) root() string {
	if s.worker.Root != "" {
		return shellQuote(s.worker.Root)
	}

	return `"${TMPDIR:-/tmp}"`
}

// probe makes the step's directory and learns what the worker is, in one round trip. The owner file sits beside the directory, never in the tree a step sees.
func (s *bareSession) probe(ctx context.Context) error {
	script := `set -e; mkdir -p ` + s.root() + `; uname -s; uname -m; id -u; id -g; ` +
		`d=$(mktemp -d ` + s.root() + `/steps-build.XXXXXX); ` +
		`printf '%s %s\n' ` + shellQuote(shell.OwnerHost()) + ` ` + strconv.Itoa(os.Getpid()) + ` > "$d.owner"; echo "$d"`

	var stdout, stderr strings.Builder

	code, err := s.exec(ctx, script, nil, &stdout, &stderr)
	if err != nil {
		return err
	}

	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if code != 0 || len(lines) != 5 {
		return fmt.Errorf("%w (exit %d): %s", errBareProbe, code, strings.TrimSpace(stderr.String()))
	}

	s.goos, s.goarch = goosOf(lines[0]), goarchOf(lines[1])
	s.uid, s.gid = atoiOrNil(lines[2]), atoiOrNil(lines[3])
	s.dir = lines[4]

	return nil
}

// sweep removes step directories a dead steps process on this machine left, judged by the owner file each was made with. Best effort.
func (s *bareSession) sweep(ctx context.Context) {
	var listing strings.Builder

	_, err := s.exec(ctx, `for o in `+s.root()+`/steps-build.*.owner; do [ -f "$o" ] && printf '%s %s\n' "$o" "$(cat "$o")"; done; true`, nil, &listing, io.Discard)
	if err != nil {
		return
	}

	var stale []string

	scanner := bufio.NewScanner(strings.NewReader(listing.String()))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 3 || fields[1] != shell.OwnerHost() {
			continue
		}

		pid, convErr := strconv.Atoi(fields[2])
		if convErr != nil || pid <= 0 || !shell.ProcessGone(pid) {
			continue
		}

		dir := strings.TrimSuffix(fields[0], ".owner")
		stale = append(stale, shellQuote(dir), shellQuote(dir+".owner"), pidFiles(dir))
	}

	if len(stale) > 0 {
		_, _ = s.exec(ctx, "rm -rf "+strings.Join(stale, " "), nil, io.Discard, io.Discard)
	}
}

func (s *bareSession) upload(ctx context.Context) error {
	if s.spec.Cwd == "" {
		return nil
	}

	err := s.untar(ctx, s.dir, func(w io.Writer) error { return wire.PackTree(w, s.spec.Cwd) })
	if err != nil {
		return fmt.Errorf("sending the step's tree: %w", err)
	}

	for name, input := range s.spec.RemoteInputs {
		err = s.uploadRemote(ctx, name, input)
		if err != nil {
			return err
		}
	}

	return nil
}

// uploadRemote brings an input another worker holds through this machine; an ssh:// worker keeps nothing to find it by.
func (s *bareSession) uploadRemote(ctx context.Context, name string, input shell.RemoteInput) error {
	staged, err := os.MkdirTemp("", "steps-pipe-")
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	defer func() { _ = os.RemoveAll(staged) }()

	_, err = Pull(ctx, shell.RunnerSpec{Worker: input.Holder}, input.Digest, staged)
	if err != nil {
		return fmt.Errorf("input %q from %s: %w", name, input.Holder, err)
	}

	err = s.untar(ctx, s.dir, func(w io.Writer) error { return wire.PackTreeAs(w, staged, name) })
	if err != nil {
		return fmt.Errorf("sending input %q: %w", name, err)
	}

	return nil
}

func (s *bareSession) untar(ctx context.Context, dir string, pack func(io.Writer) error) error {
	reader, writer := io.Pipe()

	go func() { writer.CloseWithError(pack(writer)) }()

	var stderr strings.Builder

	code, err := s.exec(ctx, "tar -xf - -C "+shellQuote(dir), &byteCounter{r: reader, n: &s.sent}, io.Discard, &stderr)

	_ = reader.CloseWithError(io.ErrClosedPipe)

	if err != nil {
		return err
	}

	if code != 0 {
		return fmt.Errorf("%w (exit %d): %s", errBareTransfer, code, strings.TrimSpace(stderr.String()))
	}

	return nil
}

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

func (s *bareSession) fetch(ctx context.Context) error {
	if s.spec.Cwd == "" || (len(s.spec.Fetch) == 0 && !s.spec.FetchAll) || shell.IsReadOnly(ctx) {
		return nil
	}

	s.fetchMu.Lock()
	defer s.fetchMu.Unlock()

	staging, err := os.MkdirTemp(filepath.Dir(s.spec.Cwd), ".steps-fetch-")
	if err != nil {
		return fmt.Errorf("fetching outputs: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()

	err = s.tarInto(ctx, s.fetchScript(), staging)
	if err != nil {
		return err
	}

	if s.spec.FetchAll {
		return replaceContents(staging, s.spec.Cwd)
	}

	return s.swapOutputs(staging)
}

func (s *bareSession) fetchScript() string {
	if s.spec.FetchAll {
		return "cd " + shellQuote(s.dir) + " && exec tar -cf - ."
	}

	quoted := make([]string, 0, len(s.spec.Fetch))
	for _, name := range s.spec.Fetch {
		quoted = append(quoted, shellQuote(name))
	}

	// An output the step never made is reported where outputs are checked, so only what exists is packed.
	return "cd " + shellQuote(s.dir) + ` && set --; for n in ` + strings.Join(quoted, " ") +
		`; do if [ -e "$n" ] || [ -L "$n" ]; then set -- "$@" "$n"; fi; done; [ $# -eq 0 ] || exec tar -cf - -- "$@"`
}

func (s *bareSession) swapOutputs(staging string) error {
	for _, name := range s.spec.Fetch {
		_, err := os.Lstat(filepath.Join(staging, name))
		if err != nil {
			continue
		}

		err = replacePath(filepath.Join(staging, name), filepath.Join(s.spec.Cwd, name))
		if err != nil {
			return err
		}
	}

	return nil
}

func (s *bareSession) tarInto(ctx context.Context, script, staging string) error {
	reader, writer := io.Pipe()

	var stderr strings.Builder

	result := make(chan error, 1)

	go func() {
		code, err := s.exec(ctx, "COPYFILE_DISABLE=1; export COPYFILE_DISABLE; "+script, nil, writer, &stderr)
		if err == nil && code != 0 {
			err = fmt.Errorf("%w (exit %d): %s", errBareTransfer, code, strings.TrimSpace(stderr.String()))
		}

		writer.CloseWithError(err)
		result <- err
	}()

	unpackErr := wire.UnpackFetchedTree(&byteCounter{r: reader, n: &s.received}, staging, fetchLimits())

	// tar pads past its end-of-archive marker; closing before the padding is read fails the remote write.
	_, _ = io.Copy(io.Discard, reader)
	_ = reader.CloseWithError(io.ErrClosedPipe)

	execErr := <-result
	if execErr != nil {
		return fmt.Errorf("fetching outputs: %w", execErr)
	}

	if unpackErr != nil {
		return fmt.Errorf("fetching outputs: %w", unpackErr)
	}

	return nil
}

// exec runs one command over a fresh session channel and reports its exit status; an error means it never ran or the connection died under it.
func (s *bareSession) exec(ctx context.Context, command string, stdin io.Reader, stdout, stderr io.Writer) (_ int, err error) {
	// Every exchange, not only the step's command: an output fetch the drop cut short races the connection reporting its end just the same.
	defer func() {
		if err != nil {
			s.conn.suspect.Store(true)
		}
	}()

	session, err := s.conn.current().NewSession()
	if err != nil {
		return 0, fmt.Errorf("opening a session: %w", err)
	}
	defer func() { _ = session.Close() }()

	session.Stdin, session.Stdout, session.Stderr = stdin, stdout, stderr

	err = session.Start(command)
	if err != nil {
		return 0, fmt.Errorf("starting %q: %w", redact.Command(command), err)
	}

	done := make(chan error, 1)

	go func() { done <- session.Wait() }()

	select {
	case err = <-done:
	case <-ctx.Done():
		_ = session.Close()
		<-done

		return 0, fmt.Errorf("%w", ctx.Err())
	}

	var exitErr *ssh.ExitError
	if errors.As(err, &exitErr) {
		if exitErr.Signal() != "" {
			return shell.SignalledExitCode, nil
		}

		return exitErr.ExitStatus(), nil
	}

	if err != nil {
		return 0, fmt.Errorf("%w", err)
	}

	return 0, nil
}

func (s *bareSession) placement() (Placement, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.dir == "" {
		return Placement{}, false
	}

	return Placement{
		Tag:           s.spec.WorkerTag,
		Address:       s.worker.Address(),
		GOOS:          s.goos,
		GOARCH:        s.goarch,
		Workdir:       s.dir,
		UID:           s.uid,
		GID:           s.gid,
		BytesSent:     s.sent.Load(),
		BytesReceived: s.received.Load(),
	}, true
}

// Its own bound: the step's context has usually just ended.
func (s *bareSession) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil
	}

	s.closed = true

	if s.conn.current() == nil {
		return nil
	}

	defer func() { _ = s.conn.current().Close() }()

	if s.dir == "" {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()

	// A drop since the last command would otherwise leave the directory, and whatever the drop interrupted, on the worker.
	_, err := s.conn.redial(ctx, s.dial)
	if err != nil {
		return fmt.Errorf("%w %q: removing %s: dialling again after the connection dropped: %w", ErrWorker, s.worker.URL, s.dir, err)
	}

	owner, pids := shellQuote(s.dir+".owner"), pidFiles(s.dir)

	// The directory stays; its owner file goes, or the next session from this machine sweeps it once this process exits.
	if s.spec.Keep {
		_, _ = s.exec(ctx, s.detachCaches()+"rm -f "+owner+" "+pids, nil, io.Discard, io.Discard)

		return nil
	}

	code, err := s.exec(ctx, `for f in `+pids+`; do p=$(cat "$f" 2>/dev/null) && kill -KILL -- -"$p" 2>/dev/null; done; `+s.detachCaches()+`rm -rf `+shellQuote(s.dir)+" "+owner+" "+pids, nil, io.Discard, io.Discard)
	if err == nil && code != 0 {
		err = fmt.Errorf("%w: rm exited %d", errBareTransfer, code)
	}

	if err != nil {
		return fmt.Errorf("%w %q: removing %s: %w", ErrWorker, s.worker.URL, s.dir, err)
	}

	return nil
}

func goosOf(uname string) string { return strings.ToLower(strings.TrimSpace(uname)) }

func goarchOf(machine string) string {
	switch strings.TrimSpace(machine) {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	default:
		return strings.TrimSpace(machine)
	}
}

func atoiOrNil(s string) *int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return nil
	}

	return &n
}
