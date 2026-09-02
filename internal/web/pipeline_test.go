package web

// The served configuration, and the complaint about the one on disk.
//
// Both are read by handlers while the daemon's reload writes them, which is
// why they live behind atomics rather than plain fields — and why the last
// test here runs readers and a writer at once, where -race is the assertion.

import (
	"errors"
	"sync"
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

func TestSetConfigSwapsWhatIsServed(t *testing.T) {
	t.Parallel()

	first := &config.Config{Name: "first"}
	second := &config.Config{Name: "second"}

	pipeline := NewPipeline("demo", "demo.yml", first, nil, nil)

	if pipeline.Config() != first {
		t.Fatal("a new pipeline does not serve the configuration it was built with")
	}

	pipeline.SetConfig(second)

	if pipeline.Config() != second {
		t.Error("SetConfig did not swap what is served")
	}
}

func TestHoldSaysWhyTheFileOnDiskIsNotServed(t *testing.T) {
	t.Parallel()

	pipeline := NewPipeline("demo", "demo.yml", &config.Config{}, nil, nil)

	if pipeline.Held() != "" {
		t.Error("a pipeline whose file loaded is complaining about it")
	}

	pipeline.Hold(errors.New("job \"build\" wants an artifact nothing produces"))

	if pipeline.Held() == "" {
		t.Fatal("a refused load left nothing for the page to say")
	}

	// A later save that works clears it: the banner must not outlive the
	// problem, or every page carries a complaint about a file that has since
	// been fixed.
	pipeline.SetConfig(&config.Config{})

	if pipeline.Held() != "" {
		t.Error("a successful load left the previous complaint standing")
	}
}

// TestConfigIsSafeUnderAReload is the reason for the atomics: handlers read
// the configuration while the reload writes it. Under -race a plain field
// here fails; without -race this test proves nothing, which is why the suite
// runs with it.
func TestConfigIsSafeUnderAReload(t *testing.T) {
	t.Parallel()

	pipeline := NewPipeline("demo", "demo.yml", &config.Config{Name: "first"}, nil, nil)

	var readers sync.WaitGroup

	for range 4 {
		readers.Add(1)

		go func() {
			defer readers.Done()

			for range 200 {
				_ = pipeline.Config().Name
				_ = pipeline.Held()
			}
		}()
	}

	for i := range 200 {
		if i%2 == 0 {
			pipeline.SetConfig(&config.Config{Name: "reloaded"})
		} else {
			pipeline.Hold(errors.New("held"))
		}
	}

	readers.Wait()
}
