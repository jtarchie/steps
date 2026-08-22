package venue

// Getting a steps binary onto a worker.
//
// Keyed by the binary's own content hash, not by its version string: the
// version is set at link time and is identical across every development build,
// so a cache keyed on it would keep executing a stale shim while somebody
// changed the code and wondered why nothing moved. A content hash cannot do
// that.

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"runtime"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/jtarchie/steps/internal/shim"
)

// shimMode is what the pushed binary is chmod'd to: executable by its owner
// and nobody else. A worker is somebody else's machine, and a world-writable
// or world-executable binary in a shared temp directory is a way in.
const shimMode = 0o700

// pushShim puts the binary on the worker if it is not already there, and
// returns the path to run.
func pushShim(client *ssh.Client, worker Worker) (string, error) {
	local, err := localBinary(worker)
	if err != nil {
		return "", err
	}

	build, err := shim.BuildOf(local)
	if err != nil {
		return "", fmt.Errorf("%w", err)
	}

	remote := remoteShimPath(worker, build)

	fs, err := sftp.NewClient(client)
	if err != nil {
		return "", fmt.Errorf("opening sftp (the worker's sshd must offer the sftp subsystem): %w", err)
	}
	defer func() { _ = fs.Close() }()

	present, err := alreadyPushed(fs, remote, local)
	if err != nil {
		return "", err
	}

	if present {
		return remote, nil
	}

	err = uploadShim(fs, local, remote)
	if err != nil {
		return "", err
	}

	return remote, nil
}

// localBinary is the binary to push: this process, or one the operator built
// for a worker whose platform this machine cannot produce.
//
// There is no cross-compilation here, deliberately. steps has no Go toolchain
// in the field, so a mismatched worker is an operator supplying a binary they
// built — which the CGO_ENABLED=0 build guard is what makes possible.
func localBinary(worker Worker) (string, error) {
	if worker.Binary != "" {
		return worker.Binary, nil
	}

	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("%w: %w", errNoBinary, err)
	}

	return self, nil
}

// alreadyPushed reports whether this exact build is on the worker.
//
// Size as well as presence: an upload interrupted partway leaves a file at the
// right path with the wrong contents, and the whole point of a content-keyed
// path is that the name is a promise about the bytes.
func alreadyPushed(fs *sftp.Client, remote, local string) (bool, error) {
	remoteInfo, err := fs.Stat(remote)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}

		return false, fmt.Errorf("checking for a pushed binary at %q: %w", remote, err)
	}

	localInfo, err := os.Stat(local)
	if err != nil {
		return false, fmt.Errorf("%w", err)
	}

	return remoteInfo.Size() == localInfo.Size(), nil
}

// uploadShim writes the binary to a temporary name and renames it into place.
//
// Never straight to the final path: two orchestrators can reach one worker at
// once, and a step must not exec a binary another is still writing. Rename is
// the atomic step, and both racers end up correct because they are writing
// identical bytes to a path named after them.
func uploadShim(fs *sftp.Client, local, remote string) error {
	err := fs.MkdirAll(path.Dir(remote))
	if err != nil {
		return fmt.Errorf("making %q on the worker: %w", path.Dir(remote), err)
	}

	suffix := make([]byte, 8)

	_, err = rand.Read(suffix)
	if err != nil {
		return fmt.Errorf("naming a temporary upload: %w", err)
	}

	staging := remote + "." + hex.EncodeToString(suffix) + ".part"

	err = writeRemote(fs, local, staging)
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

	err = fs.PosixRename(staging, remote)
	if err != nil {
		// Not every server implements the POSIX rename extension; the plain
		// one is not atomic over an existing file, which is survivable here
		// because whoever wins wrote the same bytes.
		err = fs.Rename(staging, remote)
		if err != nil && !errors.Is(err, fs2ErrExist) {
			_ = fs.Remove(staging)

			return fmt.Errorf("installing the pushed binary: %w", err)
		}
	}

	return nil
}

// fs2ErrExist is the error a non-atomic rename returns when another
// orchestrator installed the same build first, which is a race with no loser.
var fs2ErrExist = fs.ErrExist

func writeRemote(fs *sftp.Client, local, remote string) error {
	source, err := os.Open(local) //nolint:gosec // the binary this process is running, or one the operator named
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	defer func() { _ = source.Close() }()

	dest, err := fs.Create(remote)
	if err != nil {
		return fmt.Errorf("creating %q on the worker: %w", remote, err)
	}

	_, err = io.Copy(dest, source)
	if err != nil {
		_ = dest.Close()

		return fmt.Errorf("uploading the steps binary (%s/%s): %w", runtime.GOOS, runtime.GOARCH, err)
	}

	err = dest.Close()
	if err != nil {
		return fmt.Errorf("finishing the upload: %w", err)
	}

	return nil
}
