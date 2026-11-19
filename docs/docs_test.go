package docs

import (
	"strings"
	"testing"
)

// Each of these renders as a yaml example and was skipped by extract, so the example it held was never validated or run.
func TestExtractRefusesAYamlFenceItWouldSkip(t *testing.T) {
	t.Parallel()

	for _, fence := range []string{"  ```yaml", "```yml", "```YAML", "~~~yaml", "~~~ yaml test=x", "\t```yaml"} {
		_, err := extract("page.md", "intro\n"+fence+"\njobs: []\n```\n")
		if err == nil || !strings.Contains(err.Error(), "page.md:2") {
			t.Errorf("extract over %q = %v, want an error naming page.md:2", fence, err)
		}
	}

	blocks, err := extract("page.md", "```yaml test=x\njobs: []\n```\n  ```bash\n  echo\n  ```\n```go\n  ```yaml\n```\n")
	if err != nil || len(blocks) != 1 {
		t.Errorf("a column-0 yaml fence, an indented non-yaml fence and a yaml fence quoted inside another = %d blocks, %v; want 1, nil", len(blocks), err)
	}
}

// A mode word wins wherever it sits: `test=x noexec=docker` used to execute because test= came first.
func TestModeDoesNotDependOnTokenOrder(t *testing.T) {
	t.Parallel()

	for info, want := range map[string]string{
		"test=x noexec=docker": "noexec",
		"noexec=docker test=x": "noexec",
		"mcp=y fragment":       "fragment",
		"test=x":               "run",
		"":                     "run",
	} {
		if got := (Block{Info: info}).Mode(); got != want {
			t.Errorf("Mode(%q) = %q, want %q", info, got, want)
		}
	}
}
