package cli

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

// ask is askFrom on the process's stdin, which the tests that swap os.Stdin drive.
func ask(out io.Writer, question, refusal string) error {
	return askFrom(os.Stdin, out, question, refusal)
}

func (p *PipelineSetCmd) confirm(out io.Writer, name string, current Held, source string, includes map[string]string) error {
	return p.confirmFrom(os.Stdin, out, name, current, source, includes)
}

// The answer is read from the invocation's own stdin, so two invocations in one process are not both waiting on the terminal.
func TestAnAnswerComesFromTheInvocationsStdin(t *testing.T) {
	t.Parallel()

	var deletes atomic.Int32

	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && r.URL.Path == "/api/pipelines/app" {
			deletes.Add(1)
		}

		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(daemon.Close)

	var out bytes.Buffer

	err := RunWith(Streams{In: strings.NewReader("y\n"), Out: &out, Err: &out}, []string{"pipeline", "destroy", "-p", "app", "--target", daemon.URL})
	if err != nil {
		t.Fatalf("destroy answered y: %v\n%s", err, out.String())
	}

	if deletes.Load() != 1 || !strings.Contains(out.String(), "destroyed: app") {
		t.Errorf("answered y through Streams.In: %d deletes, printed:\n%s\nwant one delete and it reported", deletes.Load(), out.String())
	}
}
