package venue

// The docker+ssh:// venue: the worker's own docker daemon, driven through its socket forwarded over ssh (steps#206). No binary is pushed and no command runs over ssh; the step's tree goes into a volume, the step runs in a container that mounts it, and its outputs are read back out.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
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
	work    string
	volumes []string
	// outputs are the declared outputs that got a plain volume of their own: only those can be held, since an overlay's data is visible only while something mounts it.
	outputs map[string]string
	// fresh are the volumes this session made empty, whose roots it opens to the step's user.
	fresh []string
	// kept are volumes now named by a digest, which outlive the session.
	kept map[string]bool
	// fetchMu serializes fetches: an agent's concurrent tool calls would otherwise swap the same local paths and write kept at once.
	fetchMu   sync.Mutex
	heldMu    sync.Mutex
	held      map[string]string
	heldSizes map[string]int64

	sent, received atomic.Int64
}

func newPlusRunner(worker Worker, spec shell.RunnerSpec) (shell.Runner, error) {
	if spec.Image == "" {
		return nil, fmt.Errorf("%w %q: %w", ErrWorker, worker.URL, errNoImageOnDocker)
	}

	return plusRunner{s: &plusSession{worker: worker, spec: spec, kept: map[string]bool{}}}, nil
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

	s.evict(ctx)

	work, err := s.newVolume(ctx, "work")
	if err != nil {
		return err
	}

	s.work = work.Name

	mounts, files, err := s.compose(ctx)
	if err != nil {
		return err
	}

	err = s.openRoots(ctx)
	if err != nil {
		return err
	}

	s.holder, err = s.docker.CreateHolder(ctx, "steps-holder-"+randomSuffix(), treedigest.Image, shell.OwnershipLabels(), mounts)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	if len(files) > 0 {
		err = s.pourFiles(ctx, files)
		if err != nil {
			return err
		}
	}

	spec := s.spec
	spec.Worker, spec.WorkerTag = "", ""
	spec.DockerHost = s.daemonName()
	spec.DockerDial = dial
	spec.EnvValues = withWorkerTag(resolveEnv(spec.Env), s.spec.WorkerTag)
	spec.Volumes = mounts
	spec.MountPath = plusWorkdir

	s.inner, err = shell.NewRunner(spec)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	return nil
}

// compose lays the step's tree out as volumes: each input directory a copy-on-write view of a cached tree, each empty output a plain volume of its own, and the work volume under them for whatever else the tree holds.
func (s *plusSession) compose(ctx context.Context) ([]string, []string, error) {
	mounts := []string{volumeMount(s.work, plusWorkdir, false)}
	s.outputs = map[string]string{}

	declared := map[string]bool{}
	for _, name := range s.spec.Fetch {
		declared[name] = true
	}

	var files []string

	if s.spec.Cwd != "" {
		entries, err := os.ReadDir(s.spec.Cwd)
		if err != nil {
			return nil, nil, fmt.Errorf("%w", err)
		}

		for _, entry := range entries {
			name, local := entry.Name(), filepath.Join(s.spec.Cwd, entry.Name())

			if !entry.IsDir() {
				files = append(files, name)

				continue
			}

			mount, err := s.mountFor(ctx, name, local, declared[name])
			if err != nil {
				return nil, nil, err
			}

			mounts = append(mounts, mount)
		}
	}

	for name, input := range s.spec.RemoteInputs {
		volume, err := s.placeRemote(ctx, name, input)
		if err != nil {
			return nil, nil, err
		}

		mounts = append(mounts, volumeMount(volume, path.Join(plusWorkdir, name), false))
	}

	return mounts, files, nil
}

// mountFor places one top-level directory; an empty declared output gets a fresh volume, which is what lets it be held afterwards without a copy.
func (s *plusSession) mountFor(ctx context.Context, name, local string, output bool) (string, error) {
	target := path.Join(plusWorkdir, name)

	if output && isEmptyDir(local) {
		volume, err := s.newVolume(ctx, "out")
		if err != nil {
			return "", err
		}

		s.outputs[name] = volume.Name

		return volumeMount(volume.Name, target, false), nil
	}

	volume, err := s.placeLocal(ctx, local)
	if err != nil {
		return "", fmt.Errorf("input %q: %w", name, err)
	}

	return volumeMount(volume, target, false), nil
}

// volumeMount always says nocopy: docker otherwise fills an empty volume, mode and contents, from whatever the image has at the mount path, on every first mount, undoing openRoots.
func volumeMount(volume, target string, readOnly bool) string {
	if readOnly {
		return volume + ":" + target + ":ro,nocopy"
	}

	return volume + ":" + target + ":nocopy"
}

func isEmptyDir(dir string) bool {
	entries, err := os.ReadDir(dir)

	return err == nil && len(entries) == 0
}

// pourFiles sends the tree's top-level files, which no volume can be mounted over, into the work volume.
func (s *plusSession) pourFiles(ctx context.Context, names []string) error {
	reader, writer := io.Pipe()

	go func() { writer.CloseWithError(wire.PackPaths(writer, s.spec.Cwd, names)) }()

	err := s.docker.PutArchive(ctx, s.holder, plusWorkdir, &byteCounter{r: reader, n: &s.sent})

	_ = reader.CloseWithError(io.ErrClosedPipe)

	if err != nil {
		return fmt.Errorf("sending the step's files: %w", err)
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

	dial := func(ctx context.Context) (net.Conn, error) { return client.DialContext(ctx, "unix", s.worker.Socket) }

	s.docker, err = dockerapi.NewDialer(s.daemonName(), dial)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	// Named here: a socket nothing listens on is otherwise the first PutArchive's opaque failure.
	err = s.docker.Ping(ctx)
	if err != nil {
		return nil, fmt.Errorf("the docker daemon at %s did not answer: %w (the worker's sshd must allow AllowTcpForwarding local and AllowStreamLocalForwarding, and the ssh user must be able to open the socket — usually the docker group)", s.worker.Socket, err)
	}

	// Containers a dead steps process on this machine left on the worker; nothing else would ever reclaim them.
	shell.SweepOrphanedContainersOn(ctx, s.docker)

	if !s.docker.ImagePresent(ctx, treedigest.Image) {
		err = s.docker.Pull(ctx, treedigest.Image, io.Discard)
		if err != nil {
			return nil, fmt.Errorf("%w", err)
		}
	}

	return dial, nil
}

func (s *plusSession) newVolume(ctx context.Context, kind string) (dockerapi.Volume, error) {
	created, err := s.docker.CreateVolume(ctx, "steps-"+kind+"-"+randomSuffix(), shell.OwnershipLabels(), nil)
	if err != nil {
		return dockerapi.Volume{}, fmt.Errorf("%w", err)
	}

	s.volumes = append(s.volumes, created.Name)

	if kind != "ovl" {
		s.fresh = append(s.fresh, created.Name)
	}

	return created, nil
}

// openRoots makes every fresh volume's root writable by any user, before anything mounts it: the daemon creates them root-owned 0755, and a step with a non-root user: could not write its own outputs. An overlay takes its root from its upper, so an input dir opens up too.
//
// ponytail: an input's files arrive root-owned with the modes they had, so a non-root user: reads what is world-readable and adds to an input but cannot overwrite it; the cache is shared across users, so ownership cannot follow the step. Concourse has the same limit.
func (s *plusSession) openRoots(ctx context.Context) error {
	mounts := make([]string, 0, len(s.fresh))
	for i, name := range s.fresh {
		mounts = append(mounts, volumeMount(name, "/v/"+strconv.Itoa(i), false))
	}

	code, _, stderr, err := s.docker.RunOnce(ctx, dockerapi.ContainerSpec{
		Image:   treedigest.Image,
		Cmd:     []string{"sh", "-c", "chmod 0777 /v/*"},
		Name:    "steps-chmod-" + randomSuffix(),
		Labels:  shell.OwnershipLabels(),
		Network: "none",
		Mounts:  mounts,
	})
	if err != nil {
		return fmt.Errorf("opening the step's volumes: %w", err)
	}

	if code != 0 {
		return fmt.Errorf("opening the step's volumes: exit %d: %s", code, stderr)
	}

	return nil
}

func (s *plusSession) fetch(ctx context.Context) error {
	if s.spec.Cwd == "" || (len(s.spec.Fetch) == 0 && !s.spec.FetchAll) || shell.IsReadOnly(ctx) {
		return nil
	}

	s.fetchMu.Lock()
	defer s.fetchMu.Unlock()

	if s.spec.FetchAll {
		// ponytail: a FetchAll tree always comes home; holding it needs the work volume free of input mounts, which only a get's empty tree is.
		return s.fetchInto(ctx, plusWorkdir+"/.", s.spec.Cwd, "")
	}

	return s.fetchDeclared(ctx)
}

func (s *plusSession) fetchDeclared(ctx context.Context) error {
	held, sizes := map[string]string{}, map[string]int64{}

	for _, name := range s.spec.Fetch {
		// Digested now, so HeldOf can answer; published only at close, once nothing writes the volume.
		if volume, ok := s.outputs[name]; ok && s.spec.DeferFetch {
			digest, size, err := s.measure(ctx, volume)
			if err != nil {
				return fmt.Errorf("holding output %q: %w", name, err)
			}

			held[name] = digest
			sizes[name] = size

			continue
		}

		err := s.fetchInto(ctx, path.Join(plusWorkdir, name), s.spec.Cwd, name)
		if err != nil {
			return err
		}
	}

	s.heldMu.Lock()
	s.held, s.heldSizes = held, sizes
	s.heldMu.Unlock()

	return nil
}

func (r plusRunner) heldOutputs() (map[string]string, string, bool) {
	r.s.heldMu.Lock()
	defer r.s.heldMu.Unlock()

	if len(r.s.held) == 0 {
		return nil, "", false
	}

	return maps.Clone(r.s.held), r.s.worker.URL, true
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

	if s.docker != nil {
		errs = append(errs, s.publishHeld(ctx)...)
	}

	if s.docker != nil && !s.spec.Keep {
		errs = append(errs, s.removeOwned(ctx)...)
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

// publishHeld files each held output under the digest HeldOf reported. At close, after the step's container is gone: an alias published mid-step names a volume still being written, which a concurrent build could mount as an overlay's lower.
func (s *plusSession) publishHeld(ctx context.Context) []error {
	s.heldMu.Lock()
	held, sizes := maps.Clone(s.held), maps.Clone(s.heldSizes)
	s.heldMu.Unlock()

	var errs []error

	for name, digest := range held {
		err := s.hold(ctx, s.outputs[name], digest, sizes[name])
		if err != nil {
			errs = append(errs, fmt.Errorf("holding output %q: %w", name, err))
		}
	}

	return errs
}

// removeOwned drops what only this session used: the holder, and every volume not now named by a digest. Newest first, so an overlay goes before its upper.
func (s *plusSession) removeOwned(ctx context.Context) []error {
	var errs []error

	if s.holder != "" {
		errs = append(errs, s.docker.RemoveContainer(ctx, s.holder))
	}

	for _, name := range slices.Backward(s.volumes) {
		if !s.kept[name] {
			errs = append(errs, s.docker.RemoveVolume(ctx, name))
		}
	}

	return errs
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
