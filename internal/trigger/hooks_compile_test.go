package trigger

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	rsrc "github.com/jtarchie/steps/internal/resource"
	"github.com/jtarchie/steps/internal/webhook"
)

// countingCompile is rsrc.Receiver, counted: compiling is the cost an unsigned POST must not be able to buy.
func countingCompile(count *atomic.Int64) func(config.Resource) (*webhook.Receiver, error) {
	return func(res config.Resource) (*webhook.Receiver, error) {
		count.Add(1)

		return rsrc.Receiver(res)
	}
}

func postHook(t *testing.T, handler func(http.ResponseWriter, *http.Request, string), header http.Header, body []byte) int {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/p/test/hooks/push", bytes.NewReader(body))
	req.Header = header
	rec := httptest.NewRecorder()

	handler(rec, req, "push")

	return rec.Code
}

// TestAHookCompilesItsReceiverOncePerConfiguration: compiling per request let an unsigned POST cost every expression's compile before the signature was looked at.
func TestAHookCompilesItsReceiverOncePerConfiguration(t *testing.T) {
	f := newHookFixture(t, map[string]any{"filter": `event == "push"`, "version": map[string]any{"ref": `payload.ref`}})

	var compiles atomic.Int64

	handler := hookHandler(staticConfig(f.cfg), f.st, countingCompile(&compiles))
	body := []byte(`{"ref":"main"}`)

	for range 5 {
		if code := postHook(t, handler, signed("wrong", "d1", body), body); code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", code)
		}
	}

	if code := postHook(t, handler, signed(hookSecret, "d1", body), body); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}

	if got := compiles.Load(); got != 1 {
		t.Errorf("compiled %d times over 6 deliveries to one configuration, want 1", got)
	}
}

// TestAHookRecompilesWhenTheConfigurationChanges: a set swaps the configuration pointer, and the new one's expressions are the ones a delivery must run.
func TestAHookRecompilesWhenTheConfigurationChanges(t *testing.T) {
	f := newHookFixture(t, nil)
	strict := hookConfig(map[string]any{"filter": `payload.ref == "release"`})

	var (
		compiles atomic.Int64
		current  atomic.Pointer[config.Config]
	)

	current.Store(f.cfg)
	handler := hookHandler(current.Load, f.st, countingCompile(&compiles))
	body := []byte(`{"ref":"main"}`)

	postHook(t, handler, signed(hookSecret, "d1", body), body)
	current.Store(strict)
	postHook(t, handler, signed(hookSecret, "d2", body), body)

	if got := compiles.Load(); got != 2 {
		t.Errorf("compiled %d times across two configurations, want 2", got)
	}

	if got := f.versions(t); len(got) != 1 {
		t.Errorf("versions = %v, want only d1: the new configuration's filter drops d2", got)
	}
}

// TestConcurrentHooksShareTheCompiledReceiver is for -race: deliveries arrive concurrently while a set swaps the configuration under them.
func TestConcurrentHooksShareTheCompiledReceiver(t *testing.T) {
	f := newHookFixture(t, nil)
	configs := []*config.Config{f.cfg, hookConfig(nil)}

	var (
		calls atomic.Int64
		wg    sync.WaitGroup
	)

	handler := hookHandler(func() *config.Config { return configs[calls.Add(1)%2] }, f.st, rsrc.Receiver)
	body := []byte(`{}`)

	for range 8 {
		wg.Go(func() {
			for range 4 {
				if code := postHook(t, handler, signed("wrong", "d1", body), body); code != http.StatusUnauthorized {
					t.Errorf("status = %d, want 401", code)
				}
			}
		})
	}

	wg.Wait()
}
