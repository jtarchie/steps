package venue

// Getting a shim onto a worker.
//
// Keyed by the binary's own content hash, not by its version string: the
// version is set at link time and is identical across every development build,
// so a cache keyed on it would keep executing a stale shim while somebody
// changed the code and wondered why nothing moved. A content hash cannot do
// that.
//
// What is pushed is, in order: nothing, when ?shim= names one already there; the
// file ?binary= names; the shim embedded for the platform the worker reports;
// this process, when the worker is this machine's platform or would not say.

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/pkg/sftp/v2"
	sshfx "github.com/pkg/sftp/v2/encoding/ssh/filexfer"
	"golang.org/x/crypto/ssh"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/shim"
)

// shimMode is what the pushed binary is chmod'd to: executable by its owner
// and nobody else. A worker is somebody else's machine, and a world-writable
// or world-executable binary in a shared temp directory is a way in.
const shimMode = 0o700

// shimDirMode is the directory the pushed binary sits in, under a shared /tmp. The mode is stated rather than left to the server, which sftp v2 requires and v1 decided for us out of the remote umask; the file is what carries the 0700 above, and the directory only has to be traversable.
const shimDirMode = 0o755

// shimKind is where the shim a transport started came from, which decides
// what to tell an operator when it does not start.
type shimKind int

const (
	// kindGuess is this process's own executable, sent to a worker that
	// would not say its platform. The zero value, so a transport that never
	// said where its binary came from gets the ?binary= hint.
	kindGuess shimKind = iota
	// kindSelf is this process's own executable, on a worker that reported
	// this machine's platform.
	kindSelf
	// kindBinary is the file ?binary= names.
	kindBinary
	// kindEmbedded is a shim this steps carries for the worker's platform.
	kindEmbedded
	// kindShim is one ?shim= names, already on the worker.
	kindShim
)

// shimSource is a shim this end is about to push.
type shimSource struct {
	kind shimKind
	// name is what an error calls it: a path, or the embedded file's name.
	name  string
	build string
	size  int64
	open  func() (io.ReadCloser, error)
	// platform is the worker's goos/goarch, or "" when it would not say.
	platform string
}

// pushedShim is a shim on the worker, ready to exec.
type pushedShim struct {
	path  string
	build string
	kind  shimKind
}

// pushShim puts a shim on the worker if it is not already there, and returns
// the path to run and the build it is.
func pushShim(ctx context.Context, client *ssh.Client, worker Worker) (pushedShim, error) {
	// No probe and no sftp: the operator said it is there, and the hello's
	// protocol check is what finds out whether they were right.
	if worker.Shim != "" {
		return pushedShim{path: worker.Shim, kind: kindShim}, nil
	}

	probe := probeWorker(ctx, client)

	source, err := resolveShim(worker, probe)
	if err != nil {
		return pushedShim{}, err
	}

	remote := remoteShimPath(worker, source.build)

	fs, err := sftp.NewClient(ctx, client)
	if err != nil {
		return pushedShim{}, fmt.Errorf("opening sftp (the worker's sshd must offer the sftp subsystem): %w", err)
	}
	defer func() { _ = fs.Close() }()

	err = secureShimDirs(fs, remote, probe.uid)
	if err != nil {
		return pushedShim{}, err
	}

	present, err := alreadyPushed(fs, remote, source.size, probe.uid)
	if err != nil {
		return pushedShim{}, err
	}

	if !present {
		// Said only when bytes move: a first push over a slow link is tens of
		// seconds that otherwise read as a hang.
		events.Note(ctx, events.NoteInfo, fmt.Sprintf("worker %s: pushing shim %s for %s (%.1f MB), once per build",
			worker.Address(), path.Base(source.name), cmp.Or(source.platform, "a platform it would not name"), float64(source.size)/(1<<20)))

		err = uploadShim(fs, source, remote)
		if err != nil {
			return pushedShim{}, err
		}
	}

	return pushedShim{path: remote, build: source.build, kind: source.kind}, nil
}

// probeCommand asks the worker the two things the push needs before it has
// anything of its own running there: which binary it can exec, and who it
// is logged in as.
const probeCommand = "uname -sm; id -u"

// workerProbe is what the worker said about itself. Every field may be
// unknown: a worker with no uname or id (Windows OpenSSH, a ForceCommand) is
// pushed what it would have been pushed before the probe existed.
type workerProbe struct {
	goos, goarch string
	known        bool
	// uid is the login's numeric uid, or -1 when the worker did not say.
	uid int
}

// probeWorker runs probeCommand in its own exec channel, closed before the
// shim's opens so the two never count together against MaxSessions.
func probeWorker(ctx context.Context, client *ssh.Client) workerProbe {
	probe := workerProbe{uid: -1}

	output, err := runProbe(ctx, client)
	if err != nil {
		slog.DebugContext(ctx, "venue.probe.failed", "error", err)

		return probe
	}

	lines := strings.Split(strings.TrimSpace(output), "\n")

	// Scanning rather than taking fixed lines: a login shell's rc files may
	// print before the answer, and a missing id leaves only one line of it.
	for _, line := range lines {
		goos, goarch, ok := parsePlatform(line)
		if ok {
			probe.goos, probe.goarch, probe.known = goos, goarch, true
		}
	}

	uid, err := strconv.Atoi(strings.TrimSpace(lines[len(lines)-1]))
	if err == nil && uid >= 0 {
		probe.uid = uid
	}

	if !probe.known {
		slog.DebugContext(ctx, "venue.probe.unknown_platform", "output", output)
	}

	return probe
}

// runProbe runs probeCommand, bounded as reaching the worker is, and returns
// what it printed, capped: the output is the worker's to choose.
func runProbe(ctx context.Context, client *ssh.Client) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()

	session, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("opening a session: %w", err)
	}

	output := &diagnosticBuffer{left: diagnosticBytes}
	session.Stdout = output

	done := make(chan error, 1)

	go func() { done <- session.Run(probeCommand) }()

	select {
	case err = <-done:
	case <-ctx.Done():
		// Closing the channel is what ends a Run waiting on a worker that
		// never answers; waiting for it keeps the goroutine from outliving
		// this call.
		_ = session.Close()
		<-done

		return "", fmt.Errorf("probing the worker: %w", ctx.Err())
	}

	_ = session.Close()

	if err != nil {
		return "", fmt.Errorf("probing the worker: %w", err)
	}

	return output.String(), nil
}

// errNoShimForPlatform is a worker of a platform this steps has no shim for.
var errNoShimForPlatform = errors.New("no shim for the worker's platform")

// resolveShim picks what to push, given what the worker said about itself.
func resolveShim(worker Worker, probe workerProbe) (shimSource, error) {
	platform := ""
	if probe.known {
		platform = probe.goos + "/" + probe.goarch
	}

	if worker.Binary != "" {
		return fileSource(kindBinary, worker.Binary, platform, func() (string, error) { return buildOf(worker) })
	}

	if probe.known {
		if source, ok := embeddedSource(probe.goos, probe.goarch, platform); ok {
			return source, nil
		}
	}

	if !probe.known || (probe.goos == runtime.GOOS && probe.goarch == runtime.GOARCH) {
		self, err := os.Executable()
		if err != nil {
			return shimSource{}, fmt.Errorf("%w: %w", errNoBinary, err)
		}

		// This binary's platform, whatever the worker said: it is what the
		// upload error has to name when the worker cannot run it.
		kind := kindSelf
		if !probe.known {
			kind = kindGuess
		}

		return fileSource(kind, self, runtime.GOOS+"/"+runtime.GOARCH, func() (string, error) { return buildOf(Worker{}) })
	}

	return shimSource{}, noShimError(probe)
}

// noShimError says why nothing can be pushed, and the ways out.
func noShimError(probe workerProbe) error {
	build := fmt.Sprintf("CGO_ENABLED=0 GOOS=%s GOARCH=%s go build ./cmd/steps-shim", probe.goos, probe.goarch)

	embedded := embeddedPlatforms()
	if len(embedded) == 0 {
		return fmt.Errorf("%w: the worker is %s/%s and this steps was built without embedded shims (plain `go build`) — build steps with `task build`, or build a shim with `%s` and name it with ?binary=, or name one already on the worker with ?shim=",
			errNoShimForPlatform, probe.goos, probe.goarch, build)
	}

	return fmt.Errorf("%w: the worker is %s/%s and this steps embeds shims for %s — build one with `%s` and name it with ?binary=, or name one already on the worker with ?shim=",
		errNoShimForPlatform, probe.goos, probe.goarch, strings.Join(embedded, ", "), build)
}

// fileSource is a shim that is a file on this machine.
func fileSource(kind shimKind, name, platform string, build func() (string, error)) (shimSource, error) {
	info, err := os.Stat(name)
	if err != nil {
		return shimSource{}, fmt.Errorf("%w", err)
	}

	hash, err := build()
	if err != nil {
		return shimSource{}, err
	}

	return shimSource{
		kind:     kind,
		name:     name,
		build:    hash,
		size:     info.Size(),
		open:     func() (io.ReadCloser, error) { return os.Open(name) }, //nolint:gosec // the binary this process is running, or one the operator named
		platform: platform,
	}, nil
}

// errShimNotPrivate is a pushed-shim path somebody other than the login
// could have written.
var errShimNotPrivate = errors.New("the shim's path on the worker is not private to this login")

// checkPrivate refuses a path the login does not own, or that anyone else can
// write, or that is a symlink.
//
// The pushed binary sits at a path anyone can compute — reproducible shims
// make every build's hash public — under a root that defaults to the shared
// /tmp. A user who creates that path first would have their binary exec'd as
// the operator's login, and could have it report whatever build the hello
// asks for. Refused rather than overwritten, because a directory somebody else
// owns is one this login cannot make safe.
func checkPrivate(info fs.FileInfo, name string, uid int) error {
	if info.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s is a symlink — name a private root in the worker URL, as in ssh://host/var/tmp/steps-$USER", errShimNotPrivate, name)
	}

	attrs, ok := info.Sys().(*sshfx.Attributes)
	if ok && attrs.HasUserGroup() && int64(attrs.UID) != int64(uid) {
		return fmt.Errorf("%w: %s is owned by uid %d and this login is uid %d — name a private root in the worker URL, as in ssh://host/var/tmp/steps-$USER",
			errShimNotPrivate, name, attrs.UID, uid)
	}

	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%w: %s is writable by others (%s) — remove it on the worker, or name a private root in the worker URL, as in ssh://host/var/tmp/steps-$USER",
			errShimNotPrivate, name, info.Mode().Perm())
	}

	return nil
}

// secureShimDirs makes the two directories above a pushed shim, and refuses
// them when they are not private to this login. One this call made is
// tightened first, so a worker whose umask is 002 does not refuse its own.
// An unknown uid (the worker would not say) checks nothing, as before the
// probe existed.
func secureShimDirs(fs *sftp.Client, remote string, uid int) error {
	build := path.Dir(remote)

	for _, dir := range []string{path.Dir(build), build} {
		_, err := fs.LStat(dir)
		created := errors.Is(err, os.ErrNotExist)

		err = fs.MkdirAll(dir, shimDirMode)
		if err != nil {
			return fmt.Errorf("making %q on the worker: %w", dir, err)
		}

		if uid < 0 {
			continue
		}

		if created {
			_ = fs.Chmod(dir, shimDirMode)
		}

		info, err := fs.LStat(dir)
		if err != nil {
			return fmt.Errorf("checking %q on the worker: %w", dir, err)
		}

		err = checkPrivate(info, dir, uid)
		if err != nil {
			return err
		}
	}

	if uid < 0 {
		return nil
	}

	// Followed rather than LStat'd: macOS's /tmp is itself a symlink.
	root := path.Dir(path.Dir(build))

	info, err := fs.Stat(root)
	if err != nil {
		return fmt.Errorf("checking %q on the worker: %w", root, err)
	}

	return checkRoot(info, root, uid)
}

// checkRoot refuses a root another login could rename steps-shim/ out of and
// replace: the checks on the directories below it hold only while their
// parent keeps them where they are. Writable by others is fine when sticky
// (/tmp, /var/tmp); an owner other than this login or root is not.
func checkRoot(info fs.FileInfo, name string, uid int) error {
	attrs, ok := info.Sys().(*sshfx.Attributes)
	if ok && attrs.HasUserGroup() && attrs.UID != 0 && int64(attrs.UID) != int64(uid) {
		return fmt.Errorf("%w: the root %s is owned by uid %d and this login is uid %d — name a private root in the worker URL, as in ssh://host/var/tmp/steps-$USER",
			errShimNotPrivate, name, attrs.UID, uid)
	}

	if info.Mode().Perm()&0o022 != 0 && info.Mode()&fs.ModeSticky == 0 {
		return fmt.Errorf("%w: the root %s is writable by others (%s) without the sticky bit — name a private root in the worker URL, as in ssh://host/var/tmp/steps-$USER",
			errShimNotPrivate, name, info.Mode().Perm())
	}

	return nil
}

// localBuilds remembers the content hash of each operator-named binary, keyed
// by its path AND its identity on disk.
//
//nolint:gochecknoglobals // a cache over files, invalidated by their own stat
var localBuilds sync.Map

// buildOf is the content hash of the binary a worker runs, computed once.
//
// A session is dialled per STEP, so hashing what the comments here call a
// ~56MB binary sat on the critical path of every placed step for an answer
// that cannot change while the process lives. shim.SelfBuild already memoizes
// this process's own; an operator-named one is cached by path, on the same
// footing as the run's own executable.
func buildOf(worker Worker) (string, error) {
	if worker.Binary == "" {
		build, err := shim.SelfBuild()
		if err != nil {
			return "", fmt.Errorf("%w", err)
		}

		return build, nil
	}

	// Keyed by what the file IS, not only where it is. The map lives as long
	// as the process, and `steps web` runs for days: an operator who rebuilds
	// the binary ?binary= names got the stale hash forever, and the failure
	// was silent rather than loud — remoteShimPath is derived from that hash,
	// so the push was skipped as already-done AND checkHello compared the
	// worker against the same stale expectation and matched. The one guard
	// that catches a wrong binary on a worker was defeated by the cache that
	// fabricated what it compared against.
	key := worker.Binary

	info, err := os.Stat(worker.Binary)
	if err == nil {
		key = fmt.Sprintf("%s|%d|%d", worker.Binary, info.Size(), info.ModTime().UnixNano())
	}

	if cached, ok := localBuilds.Load(key); ok {
		return cached.(string), nil //nolint:forcetypeassert // this map holds one type
	}

	build, err := shim.BuildOf(worker.Binary)
	if err != nil {
		return "", fmt.Errorf("%w", err)
	}

	cached, _ := localBuilds.LoadOrStore(key, build)

	return cached.(string), nil //nolint:forcetypeassert // as above
}

// alreadyPushed reports whether this exact build is on the worker.
//
// Size as well as presence: an upload interrupted partway leaves a file at the
// right path with the wrong contents, and the whole point of a content-keyed
// path is that the name is a promise about the bytes.
//
// Size is a guess, not proof -- hashing the far side means running something
// on it, which is the thing this function exists to decide whether to do. The
// proof comes one step later: the shim reports its own build in the handshake
// and greet refuses a session whose answer is not what was pushed. Ownership
// is checked here rather than trusted to that proof, because a planted binary
// can report whatever build it is asked for.
func alreadyPushed(fs *sftp.Client, remote string, size int64, uid int) (bool, error) {
	remoteInfo, err := fs.LStat(remote)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}

		return false, fmt.Errorf("checking for a pushed binary at %q: %w", remote, err)
	}

	if uid >= 0 {
		err = checkPrivate(remoteInfo, remote, uid)
		if err != nil {
			return false, err
		}
	}

	return remoteInfo.Size() == size, nil
}

// uploadShim writes the binary to a temporary name and renames it into place.
//
// Never straight to the final path: two orchestrators can reach one worker at
// once, and a step must not exec a binary another is still writing. Rename is
// the atomic step, and both racers end up correct because they are writing
// identical bytes to a path named after them.
func uploadShim(fs *sftp.Client, source shimSource, remote string) error {
	suffix := make([]byte, 8)

	_, err := rand.Read(suffix)
	if err != nil {
		return fmt.Errorf("naming a temporary upload: %w", err)
	}

	staging := remote + "." + hex.EncodeToString(suffix) + ".part"

	err = writeRemote(fs, source, staging)
	if err != nil {
		// Best effort: a partial upload left behind is noise, but failing the
		// step over the cleanup would replace the real error with a worse one.
		_ = fs.Remove(staging)

		return err
	}

	// Executable BEFORE the rename, so the binary is never briefly visible at
	// its final name in a state where it could be found but not run.
	err = fs.Chmod(staging, shimMode)
	if err != nil {
		_ = fs.Remove(staging)

		return fmt.Errorf("making the pushed binary executable: %w", err)
	}

	// One call rather than the POSIX extension with a fallback: sftp v2 sends the atomic posix-rename when the server advertised it and the plain one when it did not, which is the same choice made off the handshake instead of off a failed attempt. Against a server without it the rename is not atomic over an existing file, which is survivable here because whoever wins wrote the same bytes.
	err = fs.Rename(staging, remote)
	if err != nil {
		_ = fs.Remove(staging)

		if !errors.Is(err, fs2ErrExist) {
			return fmt.Errorf("installing the pushed binary: %w", err)
		}
	}

	return nil
}

// fs2ErrExist is the error a non-atomic rename returns when another
// orchestrator installed the same build first, which is a race with no loser.
var fs2ErrExist = fs.ErrExist

func writeRemote(fs *sftp.Client, source shimSource, remote string) error {
	reader, err := source.open()
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	defer func() { _ = reader.Close() }()

	// Owner-only from creation and exclusive, not Create's 0666: on a worker
	// whose umask is permissive, another login could open the staging file
	// mid-upload, and the chmod below does not revoke a descriptor already
	// open for writing.
	dest, err := fs.OpenFile(remote, sftp.OpenFlagReadWrite|sftp.OpenFlagCreate|sftp.OpenFlagExclusive, shimMode)
	if err != nil {
		return fmt.Errorf("creating %q on the worker: %w", remote, err)
	}

	_, err = io.Copy(dest, reader)
	if err != nil {
		_ = dest.Close()

		return fmt.Errorf("uploading the shim %s (%s): %w", source.name, cmp.Or(source.platform, "platform unknown"), err)
	}

	err = dest.Close()
	if err != nil {
		return fmt.Errorf("finishing the upload: %w", err)
	}

	return nil
}
