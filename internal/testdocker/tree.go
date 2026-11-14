package testdocker

// A test's tree as a step's container sees it: in a docker volume, never a bind-mounted host path (steps#246).

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/dockerapi"
)

// TreePath is where Tree mounts the volume: the path a placed step's tree has.
const TreePath = "/steps/work"

// holderImage carries the volume for the copies in and out; never started.
const holderImage = "alpine:3"

// TreeVolume is a seeded tree: Volumes goes in a RunnerSpec beside MountPath: TreePath. A struct so a package depguard keeps off dockerapi can hold one.
type TreeVolume struct {
	Volumes []dockerapi.Bind
}

// Tree copies dir into a fresh volume. Symlinks and modes are carried as they are, so a test of what a link may reach still plants one on the host.
func Tree(t testing.TB, dir string) TreeVolume {
	t.Helper()

	client, holder, mount := treeHolder(t)

	var buf bytes.Buffer

	err := tarDir(&buf, dir)
	if err != nil {
		t.Fatalf("archiving %s: %v", dir, err)
	}

	err = client.PutArchive(t.Context(), holder, TreePath, &buf)
	if err != nil {
		t.Fatalf("seeding the tree volume: %v", err)
	}

	return TreeVolume{Volumes: []dockerapi.Bind{mount}}
}

// ReadTree is rel's bytes and mode as the volume holds them: what a step's fetch would bring home.
func ReadTree(t testing.TB, tree TreeVolume, rel string) ([]byte, fs.FileMode, error) {
	t.Helper()

	client := dial(t)

	holder, err := client.CreateHolder(t.Context(), "steps-test-read-"+suffix(t), holderImage, nil, tree.Volumes)
	if err != nil {
		t.Fatalf("creating a reader for the tree volume: %v", err)
	}

	defer func() { _ = client.RemoveContainer(context.WithoutCancel(t.Context()), holder) }()

	stream, err := client.GetArchive(t.Context(), holder, TreePath+"/"+rel)
	if err != nil {
		return nil, 0, os.ErrNotExist
	}

	defer func() { _ = stream.Close() }()

	reader := tar.NewReader(stream)

	header, err := reader.Next()
	if err != nil {
		t.Fatalf("reading %s from the tree volume: %v", rel, err)
	}

	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("reading %s from the tree volume: %v", rel, err)
	}

	return data, header.FileInfo().Mode().Perm(), nil
}

// treeHolder makes a volume, removed when the test ends, mounted at TreePath in a never-started container that PutArchive can write through.
func treeHolder(t testing.TB) (*dockerapi.Client, string, dockerapi.Bind) {
	t.Helper()

	client := dial(t)
	name := "steps-test-tree-" + suffix(t)

	_, err := client.CreateVolume(t.Context(), name, nil, nil)
	if err != nil {
		t.Fatalf("creating the tree volume: %v", err)
	}

	mount := dockerapi.Bind{Source: name, Target: TreePath}

	if !client.ImagePresent(t.Context(), holderImage) {
		err = client.Pull(t.Context(), holderImage, io.Discard)
		if err != nil {
			t.Skipf("cannot pull %s: %v", holderImage, err)
		}
	}

	holder, err := client.CreateHolder(t.Context(), name, holderImage, nil, []dockerapi.Bind{mount})
	if err != nil {
		t.Fatalf("creating the tree holder: %v", err)
	}

	// Registered after the holder so it runs first: a volume a container still names cannot be removed.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		_ = client.RemoveVolume(ctx, name)
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		_ = client.RemoveContainer(ctx, holder)
	})

	return client, holder, mount
}

// dial is a client to the daemon the code under test would reach, closed when the test ends.
func dial(t testing.TB) *dockerapi.Client {
	t.Helper()

	Require(t)

	client, err := dockerapi.New("")
	if err != nil {
		t.Fatalf("dialing the daemon: %v", err)
	}

	t.Cleanup(func() { _ = client.Close() })

	return client
}

// suffix keeps names unique across parallel tests and repeated runs.
func suffix(t testing.TB) string {
	t.Helper()

	raw := make([]byte, 6)
	_, _ = rand.Read(raw)

	return hex.EncodeToString(raw)
}

// tarDir writes dir's contents, relative to dir, as a tar stream.
func tarDir(out io.Writer, dir string) error {
	writer := tar.NewWriter(out)

	err := filepath.WalkDir(dir, func(path string, _ fs.DirEntry, walkErr error) error {
		if walkErr != nil || path == dir {
			return walkErr
		}

		return tarEntry(writer, dir, path)
	})
	if err != nil {
		return fmt.Errorf("walking %s: %w", dir, err)
	}

	return writer.Close() //nolint:wrapcheck // the caller names the directory
}

// tarEntry writes one file, directory or symlink under dir.
func tarEntry(writer *tar.Writer, dir, path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err //nolint:wrapcheck // the caller names the directory
	}

	link := ""
	if info.Mode()&fs.ModeSymlink != 0 {
		link, err = os.Readlink(path)
		if err != nil {
			return err //nolint:wrapcheck // as above
		}
	}

	header, err := tar.FileInfoHeader(info, link)
	if err != nil {
		return err //nolint:wrapcheck // as above
	}

	rel, _ := filepath.Rel(dir, path)
	header.Name = filepath.ToSlash(rel)

	err = writer.WriteHeader(header)
	if err != nil || !info.Mode().IsRegular() {
		return err //nolint:wrapcheck // as above
	}

	file, err := os.Open(path) //nolint:gosec // a test's own fixture
	if err != nil {
		return err //nolint:wrapcheck // as above
	}

	defer func() { _ = file.Close() }()

	_, err = io.Copy(writer, file)

	return err //nolint:wrapcheck // as above
}
