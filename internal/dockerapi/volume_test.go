package dockerapi

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/testsshd"
)

type entry struct {
	mode   int64
	body   string
	target string
}

func tarOf(t *testing.T, entries map[string]entry) io.Reader {
	t.Helper()

	var buf bytes.Buffer

	w := tar.NewWriter(&buf)

	for name, e := range entries {
		header := &tar.Header{Name: name, Mode: e.mode, Size: int64(len(e.body)), Typeflag: tar.TypeReg}
		if e.target != "" {
			header = &tar.Header{Name: name, Linkname: e.target, Typeflag: tar.TypeSymlink}
		}

		err := w.WriteHeader(header)
		if err != nil {
			t.Fatal(err)
		}

		_, _ = io.WriteString(w, e.body)
	}

	err := w.Close()
	if err != nil {
		t.Fatal(err)
	}

	return &buf
}

func untar(t *testing.T, r io.Reader) map[string]entry {
	t.Helper()

	entries := map[string]entry{}
	reader := tar.NewReader(r)

	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return entries
		}

		if err != nil {
			t.Fatal(err)
		}

		body, _ := io.ReadAll(reader)
		// A symlink's mode is meaningless on Linux (always 0777), so only a file's is compared.
		if header.Typeflag == tar.TypeSymlink {
			entries[header.Name] = entry{target: header.Linkname}

			continue
		}

		entries[header.Name] = entry{mode: header.Mode & 0o777, body: string(body)}
	}
}

// The pid keeps two processes running the same test (shards, a second worktree) off each other's names and label counts: the daemon is shared.
func uniqueName(t *testing.T, suffix string) string {
	return "steps-test-" + strconv.Itoa(os.Getpid()) + "-" + strings.NewReplacer("/", "-", "#", "-").Replace(t.Name()) + "-" + suffix
}

func testLabels(t *testing.T) map[string]string {
	return map[string]string{"steps.test": t.Name(), "steps.pid": strconv.Itoa(os.Getpid())}
}

func volume(t *testing.T, client *Client, suffix string, driverOpts map[string]string) Volume {
	t.Helper()

	created, err := client.CreateVolume(t.Context(), uniqueName(t, suffix), testLabels(t), driverOpts)
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}

	t.Cleanup(func() { _ = client.RemoveVolume(context.WithoutCancel(t.Context()), created.Name) })

	return created
}

func holder(t *testing.T, client *Client, suffix string, mounts ...string) string {
	t.Helper()

	if !client.ImagePresent(t.Context(), testImage) {
		t.Skipf("%s is not on this daemon; this test must not depend on a network", testImage)
	}

	id, err := client.CreateHolder(t.Context(), uniqueName(t, suffix), testImage, testLabels(t), mounts)
	if err != nil {
		t.Fatalf("CreateHolder: %v", err)
	}

	// Registered after the volumes', so it runs first: a volume in use by a container cannot be removed.
	t.Cleanup(func() { _ = client.RemoveContainer(context.WithoutCancel(t.Context()), id) })

	return id
}

// A tree poured into a volume through a holder comes back out byte for byte, exec bit and symlink included.
func TestVolumeRoundTripsATree(t *testing.T) {
	roundTrip(t, requireDaemon(t))
}

// The docker+ssh:// data plane end to end: the same round trip, the engine API reached through an ssh streamlocal channel.
func TestVolumeRoundTripsThroughAnSSHForward(t *testing.T) {
	local := requireDaemon(t)

	socket, ok := strings.CutPrefix(local.Host(), "unix://")
	if !ok {
		t.Skipf("docker endpoint %q is not a unix socket", local.Host())
	}

	server := testsshd.New(t)
	sshClient := server.Dial(t)

	client, err := NewDialer("ssh://"+server.Addr()+socket, func(context.Context) (net.Conn, error) {
		return sshClient.Dial("unix", socket)
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = client.Close() })

	roundTrip(t, client)

	if server.StreamLocals.Load() == 0 {
		t.Fatal("no request crossed the ssh forward")
	}
}

func roundTrip(t *testing.T, client *Client) {
	t.Helper()

	data := volume(t, client, "data", nil)
	id := holder(t, client, "holder", data.Name+":/in")

	sent := map[string]entry{
		"run":      {mode: 0o755, body: "#!/bin/sh\necho hi\n"},
		"notes.md": {mode: 0o644, body: "plain"},
		"latest":   {target: "notes.md"},
	}

	err := client.PutArchive(t.Context(), id, "/in", tarOf(t, sent))
	if err != nil {
		t.Fatalf("PutArchive: %v", err)
	}

	content, err := client.GetArchive(t.Context(), id, "/in/.")
	if err != nil {
		t.Fatalf("GetArchive: %v", err)
	}
	defer func() { _ = content.Close() }()

	got := untar(t, content)
	for name, want := range sent {
		if got["./"+name] != want {
			t.Errorf("%s: got %+v, want %+v", name, got["./"+name], want)
		}
	}

	listed, err := client.ListVolumes(t.Context(), testLabels(t))
	if err != nil || len(listed) != 1 || listed[0].Name != data.Name {
		t.Errorf("ListVolumes by label: %+v, %v", listed, err)
	}
}

// The copy-on-write a docker+ worker places inputs with: a child overlay sees the lower's files, and writing through it leaves the lower untouched.
func TestOverlayVolumeLeavesItsLowerAlone(t *testing.T) {
	client := requireDaemon(t)

	lower := volume(t, client, "lower", nil)
	upper := volume(t, client, "upper", nil)
	work := volume(t, client, "work", nil)

	err := client.PutArchive(t.Context(), holder(t, client, "fill", lower.Name+":/l"), "/l", tarOf(t, map[string]entry{"a": {mode: 0o644, body: "lower"}}))
	if err != nil {
		t.Fatalf("filling the lower: %v", err)
	}

	child := volume(t, client, "child", OverlayOptions(lower.Mountpoint, upper.Mountpoint, work.Mountpoint))
	childHolder := holder(t, client, "child-holder", child.Name+":/c")

	if got := readFile(t, client, childHolder, "/c/a"); got != "lower" {
		t.Fatalf("the child reads %q, want the lower's file", got)
	}

	err = client.PutArchive(t.Context(), childHolder, "/c", tarOf(t, map[string]entry{"a": {mode: 0o644, body: "child"}, "new": {mode: 0o644, body: "x"}}))
	if err != nil {
		t.Fatalf("writing through the child: %v", err)
	}

	if got := readFile(t, client, childHolder, "/c/a"); got != "child" {
		t.Fatalf("the child reads %q after its own write", got)
	}

	lowerHolder := holder(t, client, "lower-holder", lower.Name+":/l:ro")
	if got := readFile(t, client, lowerHolder, "/l/a"); got != "lower" {
		t.Fatalf("the lower now reads %q: the child wrote through it", got)
	}

	_, err = client.GetArchive(t.Context(), lowerHolder, "/l/new")
	if err == nil {
		t.Fatal("a file created in the child appeared in the lower")
	}
}

// Overlay options are only checked when the volume is mounted, so a bad one must fail there naming the volume, not create a holder that holds nothing.
func TestOverlayVolumeWithAMissingLowerFailsNamingIt(t *testing.T) {
	client := requireDaemon(t)

	upper := volume(t, client, "upper", nil)
	work := volume(t, client, "work", nil)
	child := volume(t, client, "child", OverlayOptions("/nonexistent/steps-lower", upper.Mountpoint, work.Mountpoint))

	err := client.PutArchive(t.Context(), holder(t, client, "holder", child.Name+":/c"), "/c", tarOf(t, map[string]entry{"a": {mode: 0o644, body: "x"}}))
	if err == nil || !strings.Contains(err.Error(), child.Name) {
		t.Fatalf("want an error naming %s, got %v", child.Name, err)
	}
}

func TestInspectVolumeReportsAMissingOne(t *testing.T) {
	client := requireDaemon(t)

	_, err := client.InspectVolume(t.Context(), uniqueName(t, "never-created"))
	if !IsNotFound(err) {
		t.Fatalf("want IsNotFound, got %v", err)
	}

	err = client.RemoveVolume(t.Context(), uniqueName(t, "never-created"))
	if err != nil {
		t.Fatalf("removing a volume that is already gone: %v", err)
	}
}

// A volume still mounted somewhere must refuse removal: eviction must never pull a lower out from under a running step.
func TestRemoveVolumeRefusesOneInUse(t *testing.T) {
	client := requireDaemon(t)

	data := volume(t, client, "data", nil)
	holder(t, client, "holder", data.Name+":/in")

	err := client.RemoveVolume(t.Context(), data.Name)
	if err == nil {
		t.Fatal("removed a volume a container still mounts")
	}
}

func readFile(t *testing.T, client *Client, id, path string) string {
	t.Helper()

	content, err := client.GetArchive(t.Context(), id, path)
	if err != nil {
		t.Fatalf("GetArchive %s: %v", path, err)
	}
	defer func() { _ = content.Close() }()

	for _, e := range untar(t, content) {
		return e.body
	}

	t.Fatalf("GetArchive %s: an empty archive", path)

	return ""
}

func TestListVolumesRefusesNoLabels(t *testing.T) {
	client := requireDaemon(t)

	for _, labels := range []map[string]string{nil, {}} {
		_, err := client.ListVolumes(t.Context(), labels)
		if !errors.Is(err, errNoLabels) {
			t.Errorf("ListVolumes(%v): %v, want a refusal", labels, err)
		}
	}
}

// The daemon answers a second create with the first volume, contents and options unchanged: a cache wants that, a one-off volume must use a fresh name.
func TestCreateVolumeReturnsAnExistingOneUntouched(t *testing.T) {
	client := requireDaemon(t)

	first := volume(t, client, "reused", nil)

	err := client.PutArchive(t.Context(), holder(t, client, "fill", first.Name+":/v"), "/v", tarOf(t, map[string]entry{"f": {mode: 0o644, body: "stale"}}))
	if err != nil {
		t.Fatal(err)
	}

	again, err := client.CreateVolume(t.Context(), first.Name, nil, map[string]string{"type": "tmpfs", "device": "tmpfs"})
	if err != nil {
		t.Fatalf("CreateVolume again: %v", err)
	}

	if got := readFile(t, client, holder(t, client, "read", again.Name+":/v"), "/v/f"); got != "stale" {
		t.Fatalf("the second create emptied the volume: %q", got)
	}
}
