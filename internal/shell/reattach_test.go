package shell

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/dockerapi"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/testdocker"
)

// logFrames is what the engine sends for a container's logs: each stream's bytes behind stdcopy's eight-byte header.
func logFrames(stdout, stderr string) []byte {
	var out []byte

	for stream, text := range map[byte]string{1: stdout, 2: stderr} {
		if text == "" {
			continue
		}

		header := make([]byte, 8)
		header[0] = stream
		binary.BigEndian.PutUint32(header[4:], uint32(len(text))) //nolint:gosec // a test's few bytes

		out = append(append(out, header...), text...)
	}

	return out
}

// shortFollowEngine answers a follow with less than the whole log, as Podman does for a container that already exited.
func shortFollowEngine(t *testing.T) func(w http.ResponseWriter, r *http.Request) {
	t.Helper()

	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/json"):
			writeJSONResponse(t, w, http.StatusNotFound, map[string]string{"message": "No such container"})
		case strings.HasSuffix(r.URL.Path, "/containers/create"):
			writeJSONResponse(t, w, http.StatusCreated, map[string]string{"Id": "cmd"})
		case strings.HasSuffix(r.URL.Path, "/logs") && r.URL.Query().Get("follow") == "1":
			_, _ = w.Write(logFrames("one\n", ""))
		case strings.HasSuffix(r.URL.Path, "/logs"):
			_, _ = w.Write(logFrames("one\ntwo\n", "oops\n"))
		case strings.HasSuffix(r.URL.Path, "/wait"):
			writeJSONResponse(t, w, http.StatusOK, map[string]int{"StatusCode": 3})
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}
}

// Podman ends a follow early on a container that already exited; the read after the exit code is what makes the output whole, and each byte is still shown once.
func TestCommandContainerFillsWhatAShortFollowMissed(t *testing.T) {
	t.Parallel()

	client := fakeEngine(t, shortFollowEngine(t))

	runner := DockerRunner{Image: "steps-test/fake:1", session: &dockerSession{image: "steps-test/fake:1", client: client}}
	cmd := Command{Script: "true", Capture: true, Reattach: true}
	out := cmd.Output(t.Context(), "")

	code, err := runner.runCommandContainer(t.Context(), client, cmd, out)
	if err != nil {
		t.Fatalf("runCommandContainer: %v", err)
	}

	res := out.Result(code)
	if res.Code != 3 || res.Stdout != "one\ntwo\n" || res.Stderr != "oops\n" {
		t.Errorf("got code %d stdout %q stderr %q; want 3, the whole log once", res.Code, res.Stdout, res.Stderr)
	}
}

// droppable dials this machine's daemon through connections it can cut all at once, as a network going away does.
type droppable struct {
	mu    sync.Mutex
	conns []net.Conn
}

func (d *droppable) dial(socket string) func(ctx context.Context) (net.Conn, error) {
	return func(ctx context.Context) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
		if err == nil {
			d.mu.Lock()
			d.conns = append(d.conns, conn)
			d.mu.Unlock()
		}

		return conn, err //nolint:wrapcheck // a dialer passes its error through
	}
}

func (d *droppable) drop() {
	d.mu.Lock()
	defer d.mu.Unlock()

	for _, conn := range d.conns {
		_ = conn.Close()
	}

	d.conns = nil
}

// The resume point is counted in the bytes the log carries, before a capture's bound cuts anything, so output past the bound and a line longer than the log driver's 16 KB fragments come through a drop whole and once.
func TestCommandContainerOutputSurvivesADropWhole(t *testing.T) {
	testdocker.Require(t)

	host, err := dockerapi.ResolveHost()
	if err != nil {
		t.Skipf("resolving the docker host: %v", err)
	}

	socket, ok := strings.CutPrefix(host, "unix://")
	if !ok {
		t.Skipf("docker endpoint %q is not a unix socket", host)
	}

	conns := &droppable{}

	runner, err := NewRunner(RunnerSpec{Image: testImage, DockerHost: host, DockerDial: conns.dial(socket)})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	defer CloseRunner(runner, "test")

	// 3000 numbered lines, a 40,000 byte line, a pause to drop in, then 3000 more.
	script := `i=0; while [ $i -lt 3000 ]; do echo "line $i"; i=$((i+1)); done; head -c 40000 /dev/zero | tr '\0' x; echo; echo marker; sleep 2; while [ $i -lt 6000 ]; do echo "line $i"; i=$((i+1)); done; echo tail >&2; exit 4`

	seen := make(chan struct{})
	watch := &signalOn{needle: "marker", seen: seen}

	go func() {
		<-seen
		conns.drop()
	}()

	ctx := events.WithOutput(t.Context(), events.Output{Stdout: watch, Stderr: io.Discard})

	res, err := runner.Exec(ctx, Command{Script: script, Capture: true, Limit: 1000, Reattach: true, ExitAsData: true, Stream: StreamStdout})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	if res.Code != 4 {
		t.Errorf("code = %d, want 4", res.Code)
	}

	var want strings.Builder
	for i := range 6000 {
		if i == 3000 {
			want.WriteString(strings.Repeat("x", 40000) + "\nmarker\n")
		}

		fmt.Fprintf(&want, "line %d\n", i)
	}

	if got := watch.String(); got != want.String() {
		t.Errorf("streamed %d bytes, want %d: the drop repeated or lost output", len(got), want.Len())
	}

	if !strings.Contains(res.Stdout, "truncated") {
		t.Errorf("the capture was not bounded: %d bytes", len(res.Stdout))
	}
}

// signalOn records what it is written and closes seen the first time needle is among it.
type signalOn struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	needle string
	seen   chan struct{}
	closed bool
}

func (s *signalOn) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.buf.Write(p)

	if !s.closed && strings.Contains(s.buf.String(), s.needle) {
		s.closed = true
		close(s.seen)
	}

	return len(p), nil
}

func (s *signalOn) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.buf.String()
}

// A command container runs its script as an exec would, past the image's ENTRYPOINT: moby/buildkit's takes a command as arguments to a daemon that never exits, and the step hung on it.
func TestCommandContainerRunsPastTheImagesEntrypoint(t *testing.T) {
	testdocker.Require(t)

	name := "steps-test-daemon-entrypoint:" + strings.ToLower(t.Name())

	build := exec.CommandContext(t.Context(), "docker", "build", "-q", "-t", name, "-") //nolint:gosec // a name from this test's own
	build.Stdin = strings.NewReader("FROM " + testImage + "\nENTRYPOINT [\"/bin/sh\", \"-c\", \"exec sleep 3600\"]\n")

	out, err := build.CombinedOutput()
	if err != nil {
		t.Skipf("cannot build the fixture image: %v\n%s", err, out)
	}

	t.Cleanup(func() {
		_ = exec.CommandContext(context.WithoutCancel(t.Context()), "docker", "rmi", "-f", name).Run() //nolint:gosec // as above
	})

	runner, err := NewRunner(RunnerSpec{Image: name})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	defer CloseRunner(runner, "test")

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	res, err := runner.Exec(ctx, Command{Script: "echo ran; exit 5", Capture: true, ExitAsData: true, Reattach: true})
	if err != nil || res.Code != 5 || res.Stdout != "ran\n" {
		t.Errorf("code %d stdout %q err %v; want the script's own 5 and output", res.Code, res.Stdout, err)
	}
}
