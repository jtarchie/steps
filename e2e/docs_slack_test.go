package e2e

// The fake Slack workspace each slack= doc example runs against, and what each one must have posted. The reader sees slack.com; the test points every slack-* resource's source.base_url at the fake.

import (
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jtarchie/steps/docs"
	"gopkg.in/yaml.v3"
)

// docSlackFixtures maps a slack=<id> fence to its assertion on what the workspace received.
var docSlackFixtures = map[string]func(t *testing.T, workspace *fakeSlackWorkspace){ //nolint:gochecknoglobals // read-only fixture table, like docGitHubFixtures
	"hook-dm": func(t *testing.T, workspace *fakeSlackWorkspace) {
		t.Helper()

		posted := workspace.postedMessages()
		if len(posted) != 1 || posted[0]["channel"] != "U024BE7LH" || !strings.HasPrefix(posted[0]["text"].(string), "the build job failed") { //nolint:forcetypeassert // the fake echoes a string it was sent
			t.Errorf("posted %v, want one top-level DM to U024BE7LH saying the build job failed", posted)
		}

		if _, threaded := posted[0]["thread_ts"]; threaded {
			t.Errorf("posted %v, want no thread_ts", posted[0])
		}
	},
}

var docSlackToken sync.Once //nolint:gochecknoglobals // guards one process-wide dummy token

// injectDocSlackFixture points every slack-* resource of a slack= block at a fake workspace, with the bot token the type reads set to a dummy.
func injectDocSlackFixture(t *testing.T, block docs.Block, body string) (string, *fakeSlackWorkspace) {
	t.Helper()

	id := block.SlackID()
	if id == "" {
		return body, nil
	}

	if _, ok := docSlackFixtures[id]; !ok {
		t.Fatalf("fence names slack=%s but docs_slack_test.go has no such fixture", id)
	}

	server, workspace := fakeSlack(t)

	// The block runs in a parallel subtest, where t.Setenv panics; the token is a dummy the fake never checks, so one process-wide value is harmless.
	docSlackToken.Do(func() {
		err := os.Setenv("SLACK_BOT_TOKEN", "xoxb-fake")
		if err != nil {
			t.Fatal(err)
		}
	})

	var doc map[string]any

	err := yaml.Unmarshal([]byte(body), &doc)
	if err != nil {
		t.Fatalf("block is not valid YAML: %v", err)
	}

	pointed := 0

	eachOf(doc, "resources", func(entry any) {
		resource, _ := entry.(map[string]any)
		kind, _ := resource["type"].(string)

		if !strings.HasPrefix(kind, "slack-") {
			return
		}

		source, _ := resource["source"].(map[string]any)
		source["base_url"] = server.URL
		pointed++
	})

	if pointed == 0 {
		t.Fatal("the fence names a slack fixture, but the block declares no slack-* resource")
	}

	rewritten, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatalf("re-marshal block: %v", err)
	}

	return string(rewritten), workspace
}
