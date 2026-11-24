package venue

import (
	"io"
	"net/url"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/testsshd"
)

// A tunnel that goes silent rather than closing is noticed by the keepalive, and the command it was carrying is found again once the worker answers, its output whole and its exit code its own (steps#306).
func TestDockerPlusReattachesThroughASilentTunnel(t *testing.T) {
	shortKeepalive(t)

	server := testsshd.New(t)
	address, frozen := silentProxy(t, server.Addr())

	worker := "docker+ssh://" + address + "?" + url.Values{
		"identity":   {server.Identity},
		"hostkey":    {ssh.FingerprintSHA256(server.HostKey)},
		"ssh_config": {"none"},
		"sock":       {hostDockerSocket(t)},
	}.Encode()
	runner := plusRunnerFor(t, worker, t.TempDir())

	started := make(chan struct{})

	go func() {
		<-started
		frozen.Store(true)
		time.Sleep(2 * time.Second)
		frozen.Store(false)
	}()

	ctx := events.WithOutput(t.Context(), events.Output{Stdout: &onFirstWrite{do: func() { close(started) }}, Stderr: io.Discard})

	res, err := runner.Exec(ctx, shell.Command{Script: "echo a; sleep 3; echo b; exit 3", Stream: shell.StreamStdout, Capture: true, ExitAsData: true, Reattach: true})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	if res.Code != 3 || res.Stdout != "a\nb\n" {
		t.Errorf("code %d stdout %q; want 3 and each line once", res.Code, res.Stdout)
	}
}

type onFirstWrite struct {
	once sync.Once
	do   func()
}

func (o *onFirstWrite) Write(p []byte) (int, error) {
	o.once.Do(o.do)

	return len(p), nil
}
