package e2e

// read_file on an image, end to end: what each kind of agent is actually
// handed, and what the state database keeps of it.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

// writeTestPNG writes a real 4x3 PNG and returns its path and bytes. Real,
// because read_file decodes the header for its dimensions and describes rather
// than sends an image whose header does not decode.
func writeTestPNG(t *testing.T) (string, []byte) {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, 4, 3))
	for x := range 4 {
		for y := range 3 {
			img.Set(x, y, color.RGBA{R: 255, A: 255})
		}
	}

	var buf bytes.Buffer

	err := png.Encode(&buf, img)
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "login.png")

	err = os.WriteFile(path, buf.Bytes(), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	return path, buf.Bytes()
}

// bridgeToolResult decodes a captured tools/call answer, which the stateless
// bridge may frame as one server-sent event rather than a bare JSON body.
type bridgeToolResult struct {
	Content []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Data     string `json:"data"`
		MIMEType string `json:"mimeType"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

func readBridgeToolResult(t *testing.T, path string) bridgeToolResult {
	t.Helper()

	body := readFileString(t, path)

	for line := range strings.SplitSeq(body, "\n") {
		if after, found := strings.CutPrefix(line, "data: "); found {
			body = after

			break
		}
	}

	var answer struct {
		Result bridgeToolResult `json:"result"`
	}

	err := json.Unmarshal([]byte(body), &answer)
	if err != nil {
		t.Fatalf("the bridge's answer is not a tools/call result: %v\n%s", err, body)
	}

	return answer.Result
}

// TestE2ECLIAgentReadFileSeesAnImage is the seam the feature exists for: a CLI
// agent's read_file is steps' own implementation reached over the bridge, and
// an image has to cross that boundary as MCP image content — the one shape the
// CLI hands its model as a picture — rather than as text. The stored
// transcript is checked in the same run, because the CLI echoes the image's
// base64 back on its own stream and that echo must not land in the database.
func TestE2ECLIAgentReadFileSeesAnImage(t *testing.T) {
	requireCurl(t)

	dir := t.TempDir()
	captured := filepath.Join(t.TempDir(), "read_file.json")
	source, want := writeTestPNG(t)
	encoded := base64.StdEncoding.EncodeToString(want)

	// The echo is the shape the real CLI streams for a tool result that
	// carried an image: the text block beside an image block holding the
	// bytes, base64 and all.
	echo := fmt.Sprintf(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":[`+
		`{"type":"text","text":"shots/login.png: PNG image, 4x3, attached as an image."},`+
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":%q}}]}]}}`, encoded)

	writeFakeClaude(t, strings.Join([]string{
		"echo '" + cliInitEvent("mcp__steps__read_file") + "'",
		"echo '" + cliToolUseEvent("t1", "mcp__steps__read_file", `{"path":"shots/login.png"}`) + "'",
		captureBridgeScript(captured, "read_file", `{"path":"shots/login.png"}`),
		"echo '" + echo + "'",
		"echo '" + cliResultEvent("It shows a red rectangle.", 2) + "'",
	}, "\n"))

	path := writePipeline(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: looker
  source:
    model: "@claude/sonnet"
  tools: [read_file]

jobs:
- name: triage
  plan:
  - task: attach
    outputs: [shots]
    run: cp %q shots/login.png
  - agent: looker
    inputs: [shots]
    messages:
      - What does shots/login.png show?
`, source))

	mustRun(t, path)

	assertImageAnswer(t, readBridgeToolResult(t, captured), encoded)

	transcript := storeTranscript(t, path, "looker")
	if strings.Contains(transcript, encoded) {
		t.Error("the stored transcript carries the image's base64")
	}

	if !strings.Contains(transcript, "attached as an image") {
		t.Errorf("the stored transcript lost the result's text: %s", transcript)
	}
}

// assertImageAnswer holds a bridged read_file answer to one text block that
// describes the image without its bytes, and one image block that IS them.
func assertImageAnswer(t *testing.T, answer bridgeToolResult, encoded string) {
	t.Helper()

	if answer.IsError || len(answer.Content) != 2 {
		t.Fatalf("read_file on a PNG answered %+v, want one text and one image block", answer)
	}

	text, image := answer.Content[0], answer.Content[1]

	if text.Type != "text" || !strings.Contains(text.Text, "PNG image, 4x3") || strings.Contains(text.Text, encoded) {
		t.Errorf("the first block should describe the image without its base64: %+v", text)
	}

	if image.Type != "image" || image.MIMEType != "image/png" || image.Data != encoded {
		t.Errorf("the second block is a %s %s carrying %d base64 bytes, want an image/png image carrying the file's own %d",
			image.Type, image.MIMEType, len(image.Data), len(encoded))
	}
}

// TestE2EContextPathImageArrivesAsAnImage carries a context_paths: image
// across the other seam: preparation reads it on this machine, and the opening
// request must already hold it as an image part — on the wire, for the model
// that takes images, and as a description for the one that does not.
func TestE2EContextPathImageArrivesAsAnImage(t *testing.T) {
	dir := t.TempDir()
	source, want := writeTestPNG(t)
	dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(want)

	fake := newRoutedFakeLLM(t, func(capturedRequest) turn { return says("Seen.") })

	path := writePipeline(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: looker
  source: {endpoint: %[1]s/v1/, model: anthropic/claude-sonnet-4.5, api_key_env: STEPS_TEST_AGENT_API_KEY}
- name: reader
  source: {endpoint: %[1]s/v1/, model: qwen/qwen3.7-flash, api_key_env: STEPS_TEST_AGENT_API_KEY}

jobs:
- name: triage
  plan:
  - task: attach
    outputs: [shots]
    run: cp %[2]q shots/login.png
  - agent: looker
    inputs: [shots]
    context_paths: [shots/login.png]
    messages:
      - What does it show?
  - agent: reader
    inputs: [shots]
    context_paths: [shots/login.png]
    messages:
      - What does it show?
`, fake.URL, source))

	mustRun(t, path)

	looker, reader := fake.request(1), fake.request(2)

	if !slices.Contains(looker.images(), dataURL) {
		t.Errorf("the sighted model's opening request carries no image part holding the file; images: %d", len(looker.images()))
	}

	if !looker.toolResultContains("attached as an image") {
		t.Errorf("the synthetic read_file result does not say an image follows: %v", looker.toolResults())
	}

	if len(reader.images()) != 0 {
		t.Error("the model that cannot be shown images was sent one anyway")
	}

	if !reader.toolResultContains("can't be shown images") || !reader.toolResultContains("PNG image, 4x3") {
		t.Errorf("the blind model was not told what the file is: %v", reader.toolResults())
	}

	for _, req := range []capturedRequest{looker, reader} {
		if strings.Contains(req.Raw, `"role":"tool","content":"`+dataURL) || req.toolResultContains(base64.StdEncoding.EncodeToString(want)) {
			t.Error("a tool message carried the image's base64")
		}
	}
}

// TestE2EFailoverToABlindModelDropsTheImages is the failover seam: the sighted
// primary reads an image, dies mid-run, and the fallback resumes the same
// conversation — which still holds the image part. A fallback that cannot be
// shown images would have its endpoint refuse that request outright, so the
// cascade has to carry the new source's sight across, not just its model.
func TestE2EFailoverToABlindModelDropsTheImages(t *testing.T) {
	isolatePreflightPins(t)

	dir := t.TempDir()
	source, want := writeTestPNG(t)
	dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(want)

	primary := newFakeLLM(t,
		callsTool("read_file", map[string]any{"path": "shots/login.png"}),
		failsWith(http.StatusInternalServerError),
		failsWith(http.StatusInternalServerError),
	)
	fallback := newFakeLLM(t, says("Described it."))

	path := writePipeline(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: seer
  source: {endpoint: %[1]s/v1/, model: anthropic/claude-sonnet-4.5, api_key_env: STEPS_TEST_AGENT_API_KEY}
  fallback:
  - source: {endpoint: %[2]s/v1/, model: qwen/qwen3.7-flash, api_key_env: STEPS_TEST_AGENT_API_KEY}
  tools: [read_file]

jobs:
- name: triage
  plan:
  - task: attach
    outputs: [shots]
    run: cp %[3]q shots/login.png
  - agent: seer
    inputs: [shots]
    attempts: 2
    messages:
      - What does shots/login.png show?
`, primary.URL, fallback.URL, source))

	mustRun(t, path)

	if !slices.Contains(primary.request(2).images(), dataURL) {
		t.Fatal("the sighted primary was never sent the image, so this test proves nothing about dropping it")
	}

	resumed := fallback.request(1)
	if len(resumed.images()) != 0 {
		t.Error("the blind fallback was sent the image the primary had read")
	}

	if !resumed.userMessageContains("image omitted") {
		t.Error("the image was dropped from the resumed conversation without a note saying so")
	}
}

// TestE2EFixAndSubAgentsSeeImagesByTheirOwnModel covers the two other places a
// conversation's tools are built: a task's fix: agent and a delegated child.
// Each decides what read_file returns by ITS OWN model — a sighted child of a
// blind parent is shown the image the parent could not be.
func TestE2EFixAndSubAgentsSeeImagesByTheirOwnModel(t *testing.T) {
	source, want := writeTestPNG(t)
	dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(want)
	looksAtTheScreenshot := func(req capturedRequest) turn {
		if req.historyCalled("read_file") {
			return says("done")
		}

		return callsTool("read_file", map[string]any{"path": "shots/login.png"})
	}

	t.Run("fix", func(t *testing.T) {
		fixer := newRoutedFakeLLM(t, looksAtTheScreenshot)

		path := writePipeline(t, t.TempDir(), fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: fixer
  source: {endpoint: %[1]s/v1/, model: anthropic/claude-sonnet-4.5, api_key_env: STEPS_TEST_AGENT_API_KEY}
  tools: [read_file]

jobs:
- name: build
  plan:
  - task: check
    inputs: []
    run: mkdir -p shots && cp %[2]q shots/login.png && exit 1
    fix: fixer
`, fixer.URL, source))

		_ = cli.Run([]string{"run", path, "--job", "build"}) // the task stays broken; what the fixer saw is the point

		if !slices.ContainsFunc(fixer.allRequests(), func(req capturedRequest) bool { return slices.Contains(req.images(), dataURL) }) {
			t.Error("a sighted fix agent was never shown the image it read")
		}
	})

	t.Run("sub-agent", func(t *testing.T) {
		lead := newRoutedFakeLLM(t, func(req capturedRequest) turn {
			if req.historyCalled("looker") {
				return says("done")
			}

			return callsTool("looker", map[string]any{"request": "what is in shots/login.png?"})
		})
		child := newRoutedFakeLLM(t, looksAtTheScreenshot)

		path := writePipeline(t, t.TempDir(), fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: looker
  source: {endpoint: %[2]s/v1/, model: anthropic/claude-sonnet-4.5, api_key_env: STEPS_TEST_AGENT_API_KEY}
  description: Looks at a screenshot.
  tools: [read_file]
- name: lead
  source: {endpoint: %[1]s/v1/, model: qwen/qwen3.7-flash, api_key_env: STEPS_TEST_AGENT_API_KEY}
  tools:
  - read_file
  - agent: looker
    description: Look at a screenshot.

jobs:
- name: triage
  plan:
  - task: attach
    outputs: [shots]
    run: cp %[3]q shots/login.png
  - agent: lead
    inputs: [shots]
    messages:
      - Have the looker describe shots/login.png.
`, lead.URL, child.URL, source))

		mustRun(t, path)

		if !slices.ContainsFunc(child.allRequests(), func(req capturedRequest) bool { return slices.Contains(req.images(), dataURL) }) {
			t.Error("a sighted child of a blind parent was never shown the image it read")
		}
	})
}
