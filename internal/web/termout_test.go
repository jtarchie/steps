package web

import (
	"strings"
	"testing"
)

// TestOutputFromAnUntrustedBranchStaysInert: output a PR under review printed is drawn, never obeyed — markup is text, a link is http(s) or nothing, a clipboard write or an image is dropped, and no control byte reaches the page.
func TestOutputFromAnUntrustedBranchStaysInert(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name, raw, want string
		refuse          []string
	}{
		{"markup", "\x1b[31m<script>alert(1)</script>\x1b[0m\n", "&lt;script&gt;", []string{"<script"}},
		{"javascript link", "\x1b]8;;javascript:alert(1)\x1b\\click\x1b]8;;\x1b\\\n", "click", []string{"href", "javascript"}},
		{"quoted href", "\x1b]8;;https://x.test/?a=\"onmouseover=x\x1b\\link\x1b]8;;\x1b\\\n", `href="https://x.test/?a=&#34;onmouseover=x"`, []string{`"onmouseover`}},
		{"clipboard write", "\x1b]52;c;aGk=\x07after\n", "after", []string{"aGk"}},
		{"inline image", "\x1b]1337;File=inline=1:aGk=\x07after\n", "after", []string{"<img"}},
		{"control bytes", "\x1b[1mx\x00y\x07z\u0085\n", "xyz", []string{"\x00", "\x07", "\u0085", "\x1b"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			for name, got := range map[string]string{"pre": string(outputPre(tc.raw)), "chunk": string(outputChunk(tc.raw))} {
				if !strings.Contains(got, tc.want) {
					t.Errorf("%s: %q lacks %q", name, got, tc.want)
				}

				for _, bad := range tc.refuse {
					if strings.Contains(got, bad) {
						t.Errorf("%s: %q carries %q", name, got, bad)
					}
				}
			}
		})
	}
}

// TestOutputChunksKeepTheirLineEndings: chunks are concatenated spans, so a chunk that ended a line must still end it once drawn, and a progress bar's carriage return draws its last state.
func TestOutputChunksKeepTheirLineEndings(t *testing.T) {
	t.Parallel()

	for raw, want := range map[string]string{
		"\x1b[32mok\x1b[0m\n": `<span class="term-fg32">ok</span>` + "\n",
		"10%\r50%\r":          "50%\n",
		"\x1b[32mpartial":     `<span class="term-fg32">partial</span>`,
		"plain\n":             "plain\n",
	} {
		if got := string(outputChunk(raw)); got != want {
			t.Errorf("outputChunk(%q) = %q, want %q", raw, got, want)
		}
	}
}

// TestPlainOutputKeepsItsHighlighting: output with nothing for a terminal to act on still goes through jsonPre, so a JSON document printed by a step stays coloured.
func TestPlainOutputKeepsItsHighlighting(t *testing.T) {
	t.Parallel()

	raw := `{"ok": true}` + "\n"
	if got, want := outputPre(raw), jsonPre(raw); got != want {
		t.Errorf("outputPre(%q) = %q, want jsonPre's %q", raw, got, want)
	}
}
