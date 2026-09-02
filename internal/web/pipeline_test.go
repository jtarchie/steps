package web

// The served configuration, and the complaint about the one on disk.
//
// Both are read by handlers while the daemon's reload writes them, which is
// why they live behind atomics rather than plain fields — and why the last
// test here runs readers and a writer at once, where -race is the assertion.

import (
	"errors"
	"strings"
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

// TestHighlightYAMLEscapesMarkup: a configuration is rendered as a code
// block, so whatever it contains stays text. The source is the operator's own
// file rather than a stranger's, which makes this a property worth pinning
// rather than a threat worth fearing — a pipeline that legitimately echoes
// HTML must read as YAML, not disappear into the page.
func TestHighlightYAMLEscapesMarkup(t *testing.T) {
	t.Parallel()

	rendered := string(highlightYAML("jobs:\n- name: build\n  run: echo '<script>alert(1)</script>'\n"))
	if strings.Contains(rendered, "<script>") {
		t.Errorf("a configuration's contents reached the page as markup:\n%s", rendered)
	}

	if !strings.Contains(rendered, "&lt;script&gt;") {
		t.Errorf("the configuration's own text is not shown:\n%s", rendered)
	}
}

// TestHighlightYAMLKeepsAFencedPromptInTheConfiguration is the corruption
// this page shipped with, and the reason it no longer goes through the
// markdown renderer.
//
// A block scalar at two spaces puts its content at three, and CommonMark
// closes a fenced block on a ``` indented by up to three — so a configuration
// wrapped in a synthetic ```yaml fence ENDED at the first such line, and
// everything after it rendered as markdown: a YAML comment became a heading,
// tags vanished, and half the file was silently dropped. On the one page
// whose entire promise is showing what the run was told to do.
func TestHighlightYAMLKeepsAFencedPromptInTheConfiguration(t *testing.T) {
	t.Parallel()

	rendered := string(highlightYAML("agents:\n- name: reviewer\n  system: |\n   ```\n   # not a heading\n   ```\njobs: []\n"))

	if strings.Contains(rendered, "<h1") {
		t.Errorf("a fence inside the configuration broke out into markdown:\n%s", rendered)
	}

	// Everything after the fence is still there — the corruption dropped it.
	for _, want := range []string{"# not a heading", "jobs", "reviewer"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the configuration is missing %q after the fenced block:\n%s", want, rendered)
		}
	}
}
