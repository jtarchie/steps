package pipeline

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/workspace"
)

// TestDeferrableRefusesACollectingCell: a cell captures under its coordinates
// (findings/alpha), and the collected artifact is composed HERE — a tree left
// on the worker under that path is one no consumer of findings would find.
func TestDeferrableRefusesACollectingCell(t *testing.T) {
	t.Parallel()

	rt := config.ResolvedTask{Name: "cell", Outputs: []string{"findings"}}

	if !deferrable(config.Step{}, rt) {
		t.Fatal("an ordinary task's outputs should be deferrable")
	}

	if deferrable(config.Step{OutputSubdir: "alpha"}, rt) {
		t.Error("a collecting cell's outputs were deferred; the collection is a local reader")
	}
}

// TestDeferrableRefusesAStepThatReadsItsOwnOutputsHere: every other way a
// step reads what it produced on this machine. Each is one reason, and a
// deferred output would make each of them see an empty directory.
func TestDeferrableRefusesAStepThatReadsItsOwnOutputsHere(t *testing.T) {
	t.Parallel()

	base := config.ResolvedTask{Name: "unit", Outputs: []string{"out"}}

	for _, tc := range []struct {
		name string
		rt   config.ResolvedTask
		want bool
	}{
		{name: "an ordinary task", rt: base, want: true},
		{name: "an assert with no files", rt: withAssert(base, &config.Assert{Code: new(int)}), want: true},
		{name: "an assert on files", rt: withAssert(base, &config.Assert{Files: []string{"out/f"}}), want: false},
		{name: "a fix agent", rt: withFix(base), want: false},
	} {
		if got := deferrable(config.Step{}, tc.rt); got != tc.want {
			t.Errorf("%s: deferrable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func withAssert(rt config.ResolvedTask, assert *config.Assert) config.ResolvedTask {
	rt.Assert = assert

	return rt
}

func withFix(rt config.ResolvedTask) config.ResolvedTask {
	rt.Fix = &config.FixSpec{}

	return rt
}

type recordingHolder struct {
	workspace.BuildWorkspace
	held    map[string]workspace.RemoteArtifact
	refuses string
}

func (h *recordingHolder) HoldRemote(name string, remote workspace.RemoteArtifact) error {
	if name == h.refuses {
		return errors.New("refused " + name)
	}

	h.held[name] = remote

	return nil
}

// Every output the worker kept is recorded under the name it is captured as, not just the first — and a hold the workspace refuses fails the step rather than leaving a consumer an empty directory.
func TestHoldRemoteOutputsRecordsEveryKeptOutput(t *testing.T) {
	t.Parallel()

	outputs := []string{"report", "bin", "local"}
	mapping := map[string]string{"bin": "binaries"}
	kept := map[string]string{"report": "sha-r", "bin": "sha-b"}

	ws := &recordingHolder{held: map[string]workspace.RemoteArtifact{}}

	err := holdRemoteOutputs(t.Context(), ws, outputs, mapping, kept, "ssh://w1")
	if err != nil {
		t.Fatalf("holdRemoteOutputs: %v", err)
	}

	if len(ws.held) != 2 || ws.held["report"].Digest != "sha-r" || ws.held["binaries"].Digest != "sha-b" || ws.held["binaries"].Holder != "ssh://w1" {
		t.Errorf("held %+v, want report and binaries (bin mapped) held on ssh://w1, and the output the worker did not keep left alone", ws.held)
	}

	refusing := &recordingHolder{held: map[string]workspace.RemoteArtifact{}, refuses: "binaries"}

	err = holdRemoteOutputs(t.Context(), refusing, outputs, mapping, kept, "ssh://w1")
	if err == nil || !strings.Contains(err.Error(), "refused binaries") {
		t.Errorf("a refused hold returned %v, want the workspace's error", err)
	}
}

// silentHolder is a docker+ssh:// worker whose sshd accepts and never answers, as an unreachable machine's tunnel does; reached gets each connect.
func silentHolder(t *testing.T, identity, knownHosts string, reached chan<- struct{}) string {
	t.Helper()

	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	var (
		mu    sync.Mutex
		conns []net.Conn
		done  sync.WaitGroup
	)

	done.Go(func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}

			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()

			reached <- struct{}{}
		}
	})

	t.Cleanup(func() {
		_ = listener.Close()

		done.Wait()

		for _, conn := range conns {
			_ = conn.Close()
		}
	})

	return "docker+ssh://" + listener.Addr().String() + "?" + url.Values{
		"identity":    {identity},
		"known_hosts": {knownHosts},
		"ssh_config":  {"none"},
	}.Encode()
}

// A run's pins come back from every holder at once: one holder that hangs must not take the time the others needed, nor add its wait to theirs.
func TestUnpinReachesEveryHolderAtOnce(t *testing.T) {
	old := unpinTimeout
	unpinTimeout = 2 * time.Second

	t.Cleanup(func() { unpinTimeout = old })

	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	identity := filepath.Join(dir, "id")
	knownHosts := filepath.Join(dir, "known_hosts")

	err = errors.Join(
		os.WriteFile(identity, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600),
		os.WriteFile(knownHosts, nil, 0o600),
	)
	if err != nil {
		t.Fatal(err)
	}

	reached := make(chan struct{}, 2)
	holders := &runHolders{holders: map[string]bool{
		silentHolder(t, identity, knownHosts, reached): true,
		silentHolder(t, identity, knownHosts, reached): true,
	}}

	started := time.Now()

	holders.unpin(t.Context(), "RUN")

	elapsed := time.Since(started)

	if len(reached) != 2 {
		t.Errorf("unpin reached %d of 2 holders: a hung one spent the other's time", len(reached))
	}

	if elapsed > unpinTimeout*3/2 {
		t.Errorf("unpin took %s for two hung holders bounded at %s each: they were dialled one after another", elapsed, unpinTimeout)
	}
}

// TestOnlyARunnerBackedPutIsHandedHeldInputs: an mcp: out reads its params' files from this disk, so an image: on its type must not leave them in docker.
func TestOnlyARunnerBackedPutIsHandedHeldInputs(t *testing.T) {
	t.Parallel()

	shellType := config.ResourceType{Image: "alpine:3"}
	mcpType := config.ResourceType{Image: "alpine:3", Config: config.ResourceTypeConfig{MCP: &config.MCPResourceConfig{Server: "s"}}}

	if !onWorker(config.Step{}, runnerImage(shellType)) {
		t.Error("an untagged put whose shell type has an image: runs on the implicit local: worker and should be handed held inputs")
	}

	if onWorker(config.Step{}, runnerImage(mcpType)) {
		t.Error("an untagged mcp: put would be handed held inputs it reads from this disk")
	}
}
