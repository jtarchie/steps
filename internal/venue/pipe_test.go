package venue

// Piping a held tree from one worker to another, with no store (steps#143).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/shim"
	"github.com/jtarchie/steps/internal/wire"
)

// pipeStandInEnv makes the helper-process shim a pipeStandIn for one worker
// root and a real shim for every other. By root, because the environment is
// the whole process's and the other end of a pipe has to stay real.
const pipeStandInEnv = "STEPS_TEST_PIPE_STAND_IN"

type pipeStandIn struct {
	// Root is the worker the stand-in impersonates.
	Root string `json:"root"`
	// Role is "holder", which dies one frame into a FrameGet, or "consumer",
	// which dies one frame into a foreign offer it asked for.
	Role string `json:"role"`
	// Count, when set, is a file every real shim writes the bytes it has
	// sent so far to.
	Count string `json:"count"`
	// Dials, when set, is a file every shim appends one byte to per start.
	Dials string `json:"dials"`
}

func withPipeStandIn(t *testing.T, standIn pipeStandIn) {
	t.Helper()

	config, err := json.Marshal(standIn)
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv(pipeStandInEnv, string(config))
}

func servePipeStandIn() {
	var config pipeStandIn

	_ = json.Unmarshal([]byte(os.Getenv(pipeStandInEnv)), &config)

	if config.Dials != "" {
		file, err := os.OpenFile(config.Dials, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = file.WriteString("x")
			_ = file.Close()
		}
	}

	build, err := shim.SelfBuild()
	if err != nil {
		os.Exit(1)
	}

	decoder := wire.NewDecoder(os.Stdin)

	frame, err := decoder.Read()
	if err != nil {
		os.Exit(1)
	}

	var hello wire.Hello

	_ = wire.DecodeJSON(frame, &hello)

	if hello.Root != config.Root {
		serveReplayed(frame, build, config.Count)
	}

	serveDyingEnd(decoder, frame, build, hello.Compression)
}

// serveReplayed is the real shim, handed the hello already read. Decoder
// reads exactly one frame, so nothing else of stdin was consumed.
func serveReplayed(hello wire.Frame, build, count string) {
	var replay bytes.Buffer

	_ = wire.NewEncoder(&replay).Write(hello)

	var out io.Writer = os.Stdout
	if count != "" {
		out = &countingStdout{file: count}
	}

	_ = shim.Serve(context.Background(), io.MultiReader(&replay, os.Stdin), out, shim.Options{Build: build})

	os.Exit(0)
}

// serveDyingEnd greets and then dies one frame into whichever half of a
// pipe reaches it: a holder's FrameGet, or a consumer's foreign offer.
func serveDyingEnd(decoder *wire.Decoder, hello wire.Frame, build, compression string) {
	encoder := wire.NewEncoder(os.Stdout)

	_ = encoder.WriteJSON(wire.FrameHelloOK, hello.Op, wire.HelloOK{
		Protocol: wire.Protocol, Build: build, Workdir: os.TempDir(), Compression: compression,
	})

	for {
		frame, err := decoder.Read()
		if err != nil {
			os.Exit(1)
		}

		switch frame.Type { //nolint:exhaustive // a stand-in answers only what a pipe sends it
		case wire.FrameGet:
			_ = encoder.Write(wire.Frame{Type: wire.FrameData, Op: frame.Op, Payload: []byte("half a tree")})

			os.Exit(1)
		case wire.FrameUpload:
			var upload wire.Upload

			_ = wire.DecodeJSON(frame, &upload)

			if len(upload.Artifacts) != 1 || !upload.Artifacts[0].Foreign {
				_ = encoder.Write(wire.Frame{Type: wire.FrameEnd, Op: frame.Op})

				continue
			}

			_ = encoder.Write(wire.Frame{Type: wire.FrameNeed, Op: frame.Op})
			_, _ = decoder.Read()

			os.Exit(1)
		default:
		}
	}
}

// countingStdout is stdout, recording how much has gone through it.
type countingStdout struct {
	file string
	n    int64
}

func (c *countingStdout) Write(p []byte) (int, error) {
	n, err := os.Stdout.Write(p)
	c.n += int64(n)
	_ = os.WriteFile(c.file, []byte(strconv.FormatInt(c.n, 10)), 0o600)

	return n, err //nolint:wrapcheck // stdout, passed through
}

// heldOn runs a placed producer on the worker rooted at root whose outputs
// stay there, and answers what it kept and the holder URL.
func heldOn(t *testing.T, root, command string, outputs ...string) (map[string]string, string) {
	t.Helper()

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	cwd := t.TempDir()
	for _, out := range outputs {
		mustMkdir(t, filepath.Join(cwd, out))
	}

	producer := newLocalRunner(t, shell.RunnerSpec{
		Cwd: cwd, Worker: "local:" + root + "?binary=" + self, Fetch: outputs, DeferFetch: true,
	})

	err = producer.Run(context.Background(), command)
	if err != nil {
		t.Fatalf("producer: %v", err)
	}

	held, holder, ok := HeldOf(producer)
	if !ok || len(held) != len(outputs) {
		t.Fatalf("the producer's worker kept %v, want %v", held, outputs)
	}

	return held, holder
}

// consumerOn is a placed runner on the worker rooted at root, with inputs
// held elsewhere and a result output.
func consumerOn(t *testing.T, root string, inputs map[string]shell.RemoteInput) (shell.Runner, string) {
	t.Helper()

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	cwd := t.TempDir()
	mustMkdir(t, filepath.Join(cwd, "result"))

	return newLocalRunner(t, shell.RunnerSpec{
		Cwd: cwd, Worker: "local:" + root + "?binary=" + self, Fetch: []string{"result"},
		RemoteInputs: inputs,
	}), cwd
}

// TestARemoteInputReachesTheWorkerThroughTheOrchestrator crosses the seam
// with no store: a keeps a tree, a session on b names it, and b reads it —
// piped through this end, which stages none of it.
func TestARemoteInputReachesTheWorkerThroughTheOrchestrator(t *testing.T) {
	temp := t.TempDir()
	t.Setenv("TMPDIR", temp)

	rootA, rootB := t.TempDir(), t.TempDir()

	held, holder := heldOn(t, rootA, "head -c 1048576 /dev/urandom > out/blob.bin", "out")

	consumer, cwd := consumerOn(t, rootB, map[string]shell.RemoteInput{"out": {Digest: held["out"], Holder: holder}})

	err := consumer.Run(context.Background(), "wc -c < out/blob.bin | tr -d ' ' > result/n")
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}

	if got := strings.TrimSpace(mustRead(t, filepath.Join(cwd, "result", "n"))); got != "1048576" {
		t.Errorf("the consumer read %q of the remote input", got)
	}

	if sent := sentBytes(t, consumer); sent < 1<<20 {
		t.Errorf("this end relayed %d bytes, want the megabyte a held", sent)
	}

	for _, pattern := range []string{"steps-wire-*", ".steps-pull-*"} {
		staged, _ := filepath.Glob(filepath.Join(temp, pattern))
		if len(staged) != 0 {
			t.Errorf("the tree was staged on this machine: %v", staged)
		}
	}

	_, statErr := os.Stat(filepath.Join(cwd, "out"))
	if statErr == nil {
		t.Error("the tree landed in the consumer's local step directory")
	}
}

// TestARemoteInputOnTheStepsOwnWorkerIsNotPiped: the holder IS the step's
// worker, so the offer is answered from its cache and nothing crosses.
func TestARemoteInputOnTheStepsOwnWorkerIsNotPiped(t *testing.T) {
	root := t.TempDir()

	held, holder := heldOn(t, root, "head -c 1048576 /dev/urandom > out/blob.bin", "out")

	dials := filepath.Join(t.TempDir(), "dials")
	withPipeStandIn(t, pipeStandIn{Dials: dials})

	consumer, cwd := consumerOn(t, root, map[string]shell.RemoteInput{"out": {Digest: held["out"], Holder: holder}})

	err := consumer.Run(context.Background(), "wc -c < out/blob.bin | tr -d ' ' > result/n")
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}

	if got := strings.TrimSpace(mustRead(t, filepath.Join(cwd, "result", "n"))); got != "1048576" {
		t.Errorf("the consumer read %q of its own worker's tree", got)
	}

	if sent := sentBytes(t, consumer); sent >= 1<<19 {
		t.Errorf("this end relayed %d bytes for a tree the worker already held", sent)
	}

	if got := mustRead(t, dials); got != "x" {
		t.Errorf("%d shims started, want the consumer's alone — the holder was dialled for nothing", len(got))
	}
}

// TestTwoRemoteInputsFromOneHolderDialItOnce: on aws:// a dial is an SSM
// bootstrap, so two inputs from one worker share one session to it.
func TestTwoRemoteInputsFromOneHolderDialItOnce(t *testing.T) {
	rootA, rootB := t.TempDir(), t.TempDir()

	held, holder := heldOn(t, rootA, "echo one > one/f && echo two > two/f", "one", "two")

	dials := filepath.Join(t.TempDir(), "dials")
	withPipeStandIn(t, pipeStandIn{Dials: dials})

	consumer, cwd := consumerOn(t, rootB, map[string]shell.RemoteInput{
		"one": {Digest: held["one"], Holder: holder},
		"two": {Digest: held["two"], Holder: holder},
	})

	err := consumer.Run(context.Background(), "cat one/f two/f > result/both")
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}

	if got := mustRead(t, filepath.Join(cwd, "result", "both")); got != "one\ntwo\n" {
		t.Errorf("the consumer read %q", got)
	}

	// The consumer and the holder, once each.
	if got := mustRead(t, dials); got != "xx" {
		t.Errorf("%d shims started, want 2 — the holder was dialled per input", len(got))
	}
}

// TestAHolderThatDiesMidPipeFailsTheConsumerByName: half a tree has reached
// the consumer when its holder dies. The step fails naming the input and the
// holder, and the consumer keeps none of it.
func TestAHolderThatDiesMidPipeFailsTheConsumerByName(t *testing.T) {
	rootA, rootB := t.TempDir(), t.TempDir()

	held, holder := heldOn(t, rootA, "head -c 1048576 /dev/urandom > out/blob.bin", "out")

	withPipeStandIn(t, pipeStandIn{Root: rootA, Role: "holder"})

	consumer, _ := consumerOn(t, rootB, map[string]shell.RemoteInput{"out": {Digest: held["out"], Holder: holder}})

	// A pipe that never ends the consumer's operation hangs rather than
	// fails, so the bound is what turns that regression red.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	err := consumer.Run(ctx, "cat out/blob.bin > /dev/null")
	if err == nil {
		t.Fatal("the consumer ran with its holder dead mid-pipe")
	}

	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the pipe hung until the deadline: %v", err)
	}

	for _, want := range []string{`"out"`, holder, "could not be piped"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}

	_, statErr := os.Stat(filepath.Join(rootB, "steps-shim", "artifacts", held["out"]))
	if statErr == nil {
		t.Error("the consumer cached half a tree under the whole tree's digest")
	}
}

// TestAConsumerThatDiesMidPipeStopsReadingTheHolder: a dead consumer stops
// the holder at once, rather than after the rest of a tree nobody will read.
func TestAConsumerThatDiesMidPipeStopsReadingTheHolder(t *testing.T) {
	const total = 64 << 20

	rootA, rootB := t.TempDir(), t.TempDir()

	held, holder := heldOn(t, rootA, "head -c "+strconv.Itoa(total)+" /dev/urandom > out/blob.bin", "out")

	count := filepath.Join(t.TempDir(), "count")
	withPipeStandIn(t, pipeStandIn{Root: rootB, Role: "consumer", Count: count})

	consumer, _ := consumerOn(t, rootB, map[string]shell.RemoteInput{"out": {Digest: held["out"], Holder: holder}})

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	err := consumer.Run(ctx, "cat out/blob.bin > /dev/null")
	if err == nil {
		t.Fatal("the consumer ran although it died mid-pipe")
	}

	if !strings.Contains(err.Error(), "stopped receiving") || !strings.Contains(err.Error(), rootB) {
		t.Errorf("error %q does not name the consumer", err)
	}

	sent, _ := strconv.ParseInt(mustRead(t, count), 10, 64)
	if sent >= total/2 {
		t.Errorf("the holder sent %d of %d bytes to a consumer that was already dead", sent, total)
	}
}

// TestPipeRefusesMismatchedCompression: the bytes are relayed as they are,
// so ends that agreed different encodings would hand one the other's.
func TestPipeRefusesMismatchedCompression(t *testing.T) {
	t.Parallel()

	consumer := &session{compression: wire.CompressionZstd}
	holders := map[string]*session{"local:elsewhere": {}}

	holdErr, sendErr := consumer.relayFromHolder(context.Background(), "out",
		shell.RemoteInput{Digest: "d", Holder: "local:elsewhere"}, holders, 1)

	if !errors.Is(holdErr, wire.ErrProtocol) || sendErr != nil {
		t.Errorf("relayFromHolder = %v, %v; want a protocol refusal before anything moved", holdErr, sendErr)
	}
}
