package venue

// The docker+ssh:// venue: the worker's own docker daemon, driven through its socket forwarded over ssh (steps#206). No binary is pushed and no command runs over ssh; the step's tree goes into a volume, the step runs in a container that mounts it, and its outputs are read back out.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"sync"
	"sync/atomic"

	"golang.org/x/crypto/ssh"

	"github.com/jtarchie/steps/internal/dockerapi"
	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/treedigest"
	"github.com/jtarchie/steps/internal/wire"
)

// plusWorkdir is where a step's tree is mounted in its container; a placed step never sees a local path, so one fixed path serves every worker.
const plusWorkdir = "/steps/work"

var (
	errNoImageOnDocker = errors.New("a docker+ worker runs every step in a container, and this step names no image")
	errPlusClosed      = errors.New("the worker session is closed")
)

// plusRunner holds its session by pointer so a WithLabel copy shares one worker.
type plusRunner struct {
	label string
	s     *plusSession
}

type plusSession struct {
	worker Worker
	spec   shell.RunnerSpec

	mu        sync.Mutex
	attempted bool
	startErr  error
	closed    bool

	ssh     *ssh.Client
	docker  *dockerapi.Client
	inner   shell.Runner
	holder  string
	volumes []string

	sent, received atomic.Int64
}

func newPlusRunner(worker Worker, spec shell.RunnerSpec) (shell.Runner, error) {
	if spec.Image == "" {
		return nil, fmt.Errorf("%w %q: %w", ErrWorker, worker.URL, errNoImageOnDocker)
	}

	// ponytail: an input held on another worker needs the 2d pipe; refused until then rather than run against a tree missing it.
	if len(spec.RemoteInputs) > 0 {
		return nil, fmt.Errorf("%w %q: %w", ErrWorker, worker.URL, errRemoteInputsHere)
	}

	return plusRunner{s: &plusSession{worker: worker, spec: spec}}, nil
}

func (r plusRunner) Run(ctx context.Context, command string) error {
	return r.around(ctx, func(inner shell.Runner) error { return inner.Run(ctx, command) })
}

func (r plusRunner) RunStreamedCapture(ctx context.Context, command string, maxBytes int) (string, string, error) {
	var stdout, stderr string

	err := r.around(ctx, func(inner shell.Runner) error {
		var runErr error

		stdout, stderr, runErr = inner.RunStreamedCapture(ctx, command, maxBytes)

		return runErr //nolint:wrapcheck // shell's error is the step's
	})

	return stdout, stderr, err
}

func (r plusRunner) RunCapture(ctx context.Context, command string) ([]byte, error) {
	var stdout []byte

	err := r.around(ctx, func(inner shell.Runner) error {
		var runErr error

		stdout, runErr = inner.RunCapture(ctx, command)

		return runErr //nolint:wrapcheck // shell's error is the step's
	})

	return stdout, err
}

func (r plusRunner) RunCaptureFull(ctx context.Context, command string) (string, string, int, error) {
	return r.full(ctx, func(inner shell.Runner) (string, string, int, error) { return inner.RunCaptureFull(ctx, command) })
}

func (r plusRunner) RunCaptureFullLimited(ctx context.Context, command string, maxBytes int, spillDir string) (string, string, int, error) {
	return r.full(ctx, func(inner shell.Runner) (string, string, int, error) {
		return inner.RunCaptureFullLimited(ctx, command, maxBytes, spillDir)
	})
}

func (r plusRunner) RunCaptureFullLimitedStreamed(ctx context.Context, command string, maxBytes int, spillDir string) (string, string, int, error) {
	return r.full(ctx, func(inner shell.Runner) (string, string, int, error) {
		return inner.RunCaptureFullLimitedStreamed(ctx, command, maxBytes, spillDir)
	})
}

func (r plusRunner) WithLabel(label string) shell.Runner {
	r.label = label

	return r
}

func (r plusRunner) Close() error { return r.s.close() }

func (r plusRunner) placement() (Placement, bool) { return r.s.placement() }

func (r plusRunner) full(ctx context.Context, run func(shell.Runner) (string, string, int, error)) (string, string, int, error) {
	var (
		stdout, stderr string
		code           int
	)

	err := r.around(ctx, func(inner shell.Runner) error {
		var runErr error

		stdout, stderr, code, runErr = run(inner)

		return runErr
	})

	return stdout, stderr, code, err
}

// around runs one command and then brings the outputs home, even after a nonzero exit, because the local tree must reflect the worker the moment a Run* returns (an assert: reads it).
func (r plusRunner) around(ctx context.Context, run func(shell.Runner) error) error {
	inner, err := r.s.ensure(ctx)
	if err != nil {
		return err
	}

	if r.label != "" {
		inner = inner.WithLabel(r.label)
	}

	runErr := run(inner)
	if runErr != nil && !shell.IsExitError(runErr) {
		return runErr
	}

	err = r.s.fetch(ctx)
	if err != nil {
		return err
	}

	return runErr
}

// A failure sticks: a dead worker costs one timeout, not one per command.
func (s *plusSession) ensure(ctx context.Context) (shell.Runner, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil, fmt.Errorf("%w %q: %w", ErrWorker, s.worker.URL, errPlusClosed)
	}

	if !s.attempted {
		s.attempted = true

		err := s.connect(ctx)
		if err != nil {
			s.startErr = fmt.Errorf("%w %q: %w", ErrWorker, s.worker.URL, err)
		}
	}

	return s.inner, s.startErr
}

func (s *plusSession) daemonName() string { return s.worker.Address() + ":" + s.worker.Socket }

func (s *plusSession) connect(ctx context.Context) error {
	dial, err := s.dialDaemon(ctx)
	if err != nil {
		return err
	}

	work, err := s.newVolume(ctx, "work")
	if err != nil {
		return err
	}

	s.holder, err = s.docker.CreateHolder(ctx, "steps-holder-"+randomSuffix(), treedigest.Image, shell.OwnershipLabels(), []string{work + ":" + plusWorkdir})
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	if s.spec.Cwd != "" {
		err = s.upload(ctx)
		if err != nil {
			return err
		}
	}

	// ponytail: the shim path sweeps orphaned containers on the worker's daemon first; that sweep takes a host string, and this daemon is reached by dialer only.
	spec := s.spec
	spec.Worker, spec.WorkerTag = "", ""
	spec.DockerHost = s.daemonName()
	spec.DockerDial = dial
	spec.EnvValues = withWorkerTag(resolveEnv(spec.Env), s.spec.WorkerTag)
	spec.Volumes = []string{work + ":" + plusWorkdir}
	spec.MountPath = plusWorkdir

	s.inner, err = shell.NewRunner(spec)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	return nil
}

// dialDaemon reaches the daemon and readies the busybox every holder and digest runs on, all before any file moves.
func (s *plusSession) dialDaemon(ctx context.Context) (func(context.Context) (net.Conn, error), error) {
	client, err := sshClientFor(ctx, s.worker)
	if err != nil {
		return nil, err
	}

	s.ssh = client

	dial := func(context.Context) (net.Conn, error) { return client.Dial("unix", s.worker.Socket) }

	s.docker, err = dockerapi.NewDialer(s.daemonName(), dial)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	// Named here: a socket nothing listens on is otherwise the first PutArchive's opaque failure.
	err = s.docker.Ping(ctx)
	if err != nil {
		return nil, fmt.Errorf("the docker daemon at %s did not answer: %w", s.worker.Socket, err)
	}

	if !s.docker.ImagePresent(ctx, treedigest.Image) {
		err = s.docker.Pull(ctx, treedigest.Image, io.Discard)
		if err != nil {
			return nil, fmt.Errorf("%w", err)
		}
	}

	return dial, nil
}

func (s *plusSession) newVolume(ctx context.Context, kind string) (string, error) {
	created, err := s.docker.CreateVolume(ctx, "steps-"+kind+"-"+randomSuffix(), shell.OwnershipLabels(), nil)
	if err != nil {
		return "", fmt.Errorf("%w", err)
	}

	s.volumes = append(s.volumes, created.Name)

	return created.Name, nil
}

func (s *plusSession) upload(ctx context.Context) error {
	reader, writer := io.Pipe()

	go func() { writer.CloseWithError(wire.PackTree(writer, s.spec.Cwd)) }()

	counted := &byteCounter{r: reader, n: &s.sent}

	err := s.docker.PutArchive(ctx, s.holder, plusWorkdir, counted)

	_ = reader.CloseWithError(io.ErrClosedPipe)

	if err != nil {
		return fmt.Errorf("sending the step's tree: %w", err)
	}

	return nil
}

func (s *plusSession) fetch(ctx context.Context) error {
	if s.spec.Cwd == "" || (len(s.spec.Fetch) == 0 && !s.spec.FetchAll) || shell.IsReadOnly(ctx) {
		return nil
	}

	if s.spec.FetchAll {
		return s.fetchInto(ctx, plusWorkdir+"/.", s.spec.Cwd, "")
	}

	for _, name := range s.spec.Fetch {
		err := s.fetchInto(ctx, path.Join(plusWorkdir, name), s.spec.Cwd, name)
		if err != nil {
			return err
		}
	}

	return nil
}

// fetchInto unpacks src beside dst first and swaps it in, so a failed transfer never leaves an output half-replaced; name empty means dst's whole contents.
func (s *plusSession) fetchInto(ctx context.Context, src, dst, name string) error {
	content, err := s.docker.GetArchive(ctx, s.holder, src)
	if dockerapi.IsNotFound(err) {
		// An output the step never produced is reported where outputs are checked, not here.
		return nil
	}

	if err != nil {
		return fmt.Errorf("fetching %s: %w", src, err)
	}
	defer func() { _ = content.Close() }()

	staging, err := os.MkdirTemp(filepath.Dir(dst), ".steps-fetch-")
	if err != nil {
		return fmt.Errorf("fetching %s: %w", src, err)
	}
	defer func() { _ = os.RemoveAll(staging) }()

	err = wire.UnpackFetchedTree(&byteCounter{r: content, n: &s.received}, staging)
	if err != nil {
		return fmt.Errorf("fetching %s: %w", src, err)
	}

	if name != "" {
		return replacePath(filepath.Join(staging, name), filepath.Join(dst, name))
	}

	return replaceContents(staging, dst)
}

func replacePath(from, to string) error {
	err := os.RemoveAll(to)
	if err != nil {
		return fmt.Errorf("replacing %s: %w", to, err)
	}

	err = os.Rename(from, to)
	if err != nil {
		return fmt.Errorf("replacing %s: %w", to, err)
	}

	return nil
}

func replaceContents(from, to string) error {
	existing, err := os.ReadDir(to)
	if err != nil {
		return fmt.Errorf("replacing %s: %w", to, err)
	}

	for _, entry := range existing {
		err = os.RemoveAll(filepath.Join(to, entry.Name()))
		if err != nil {
			return fmt.Errorf("replacing %s: %w", to, err)
		}
	}

	staged, err := os.ReadDir(from)
	if err != nil {
		return fmt.Errorf("replacing %s: %w", to, err)
	}

	for _, entry := range staged {
		err = os.Rename(filepath.Join(from, entry.Name()), filepath.Join(to, entry.Name()))
		if err != nil {
			return fmt.Errorf("replacing %s: %w", to, err)
		}
	}

	return nil
}

func (s *plusSession) placement() (Placement, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.inner == nil {
		return Placement{}, false
	}

	return Placement{
		Tag:           s.spec.WorkerTag,
		Address:       s.worker.Address(),
		Workdir:       plusWorkdir,
		Image:         s.spec.Image,
		BytesSent:     s.sent.Load(),
		BytesReceived: s.received.Load(),
	}, true
}

// Its own bound, never the caller's: the likeliest reason this runs is that the step's context just ended.
func (s *plusSession) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil
	}

	s.closed = true

	ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()

	var errs []error

	if s.inner != nil {
		errs = append(errs, s.inner.Close())
	}

	if s.docker != nil && !s.spec.Keep {
		if s.holder != "" {
			errs = append(errs, s.docker.RemoveContainer(ctx, s.holder))
		}

		for _, name := range s.volumes {
			errs = append(errs, s.docker.RemoveVolume(ctx, name))
		}
	}

	if s.docker != nil {
		errs = append(errs, s.docker.Close())
	}

	if s.ssh != nil {
		_ = s.ssh.Close()
	}

	err := errors.Join(errs...)
	if err != nil {
		return fmt.Errorf("%w %q: releasing: %w", ErrWorker, s.worker.URL, err)
	}

	return nil
}

type byteCounter struct {
	r io.Reader
	n *atomic.Int64
}

func (c *byteCounter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))

	return n, err //nolint:wrapcheck // a Reader passes its source's EOF through untouched
}

func randomSuffix() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)

	return hex.EncodeToString(b)
}
