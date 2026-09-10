package web

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/workspace"
)

// Counted rather than observed through the filesystem, because the question is whether Close was reached, not what it removed.
//
// `entered` is closed as Close BEGINS, before `slow` delays it. Retirement is a
// background goroutine, so a test that reads the count straight after the call
// that starts one is reading before it has been scheduled — which is a test
// that passes whatever the implementation does. Every assertion below waits on
// this channel instead.
type countingProvider struct {
	mu      sync.Mutex
	closed  int
	entered chan struct{}
	once    sync.Once
	slow    time.Duration
}

func newCountingProvider() *countingProvider {
	return &countingProvider{entered: make(chan struct{})}
}

func (p *countingProvider) Validate() error { return nil }

func (p *countingProvider) NewBuild(context.Context, string) (workspace.BuildWorkspace, error) {
	return nil, nil //nolint:nilnil // never called: these tests only lease and retire
}

func (p *countingProvider) Close() error {
	p.once.Do(func() { close(p.entered) })

	// Held open long enough that a shutdown which does NOT wait for retirements
	// returns while this is still running, and is caught reading a zero.
	time.Sleep(p.slow)

	p.mu.Lock()
	defer p.mu.Unlock()

	p.closed++

	return nil
}

func (p *countingProvider) closes() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.closed
}

// Close removes the tree it created, so retiring one under a build in flight deletes that build's own workspace.
func TestAReplacedProviderOutlivesTheRunHoldingIt(t *testing.T) {
	t.Parallel()

	held := newProviders()
	first := newCountingProvider()
	held.set("demo", first)

	provider, release, ok := held.take("demo")
	if !ok || provider != workspace.Provider(first) {
		t.Fatalf("take gave %v, %v; want the provider that was set", provider, ok)
	}

	held.set("demo", newCountingProvider())

	// Waited on rather than sampled: a retirement that ignores the lease closes
	// within microseconds of the set above, while a correct one never closes at
	// all until the release below — so this window separates them, and reading
	// the count here would only report which goroutine happened to run first.
	select {
	case <-first.entered:
		t.Fatal("the provider a run is holding was closed while it was still leased")
	case <-time.After(leaseWindow):
	}

	release()

	waitForClose(t, first)
}

// leaseWindow is how long a retirement that ignores its lease is given to prove
// it, which it does immediately or not at all.
const leaseWindow = 250 * time.Millisecond

// Only a provider's own Close removes the tree it created, and a shutdown that outran a retirement already in flight leaked one.
func TestCloseRetiresEveryProviderIncludingTheOnesAlreadyRetiring(t *testing.T) {
	t.Parallel()

	held := newProviders()

	// Slow, because that is what makes the assertion mean anything: a Close
	// that does not wait for retirements already in flight returns while this
	// one is still inside its own, and is caught having left it unclosed.
	superseded := newCountingProvider()
	superseded.slow = leaseWindow

	current := newCountingProvider()
	other := newCountingProvider()

	held.set("demo", superseded)

	_, release, _ := held.take("demo")

	held.set("demo", current)
	held.set("infra", other)

	// Released first, so the retirement is genuinely IN FLIGHT — waiting inside
	// its own Close — rather than not yet started, which every shutdown would
	// survive.
	release()
	<-superseded.entered

	held.Close()

	for name, provider := range map[string]*countingProvider{
		"superseded": superseded, "current": current, "other": other,
	} {
		if got := provider.closes(); got != 1 {
			t.Errorf("the %s provider was closed %d times; want 1", name, got)
		}
	}
}

// What `steps pipeline destroy` leans on.
func TestRemoveRetiresADestroyedPipelinesProvider(t *testing.T) {
	t.Parallel()

	held := newProviders()
	provider := newCountingProvider()
	held.set("demo", provider)
	held.remove("demo")

	waitForClose(t, provider)

	if _, _, ok := held.take("demo"); ok {
		t.Error("take still hands out a removed pipeline's provider")
	}
}

// Retirement is deliberately in the background, so this polls rather than asserting once.
func waitForClose(t *testing.T, provider *countingProvider) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if provider.closes() == 1 {
			return
		}

		time.Sleep(time.Millisecond)
	}

	t.Fatalf("the provider was closed %d times; want 1", provider.closes())
}
