package web

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var themeNames = [2]string{"light", "dark"}

// themeTokens reads app.css's custom properties once per theme: index 0 is the light side of every light-dark(), index 1 the dark. A color token that is not a light-dark() pair is an error, because a plain value would be the same color in both themes and only one of them was designed for it.
func themeTokens(t *testing.T, css []byte) [2]map[string]string {
	t.Helper()

	raw := map[string]string{}
	for _, m := range regexp.MustCompile(`(?m)^\s*--([a-z0-9-]+):\s*([^;]+);`).FindAllStringSubmatch(string(css), -1) {
		raw[m[1]] = strings.TrimSpace(m[2])
	}

	pair := regexp.MustCompile(`^light-dark\(\s*([^,]+?)\s*,\s*([^)]+?)\s*\)$`)
	themes := [2]map[string]string{{}, {}}

	for name := range raw {
		value := raw[name]
		for strings.HasPrefix(value, "var(--") {
			value = raw[strings.TrimSuffix(strings.TrimPrefix(value, "var(--"), ")")]
		}

		switch m := pair.FindStringSubmatch(value); {
		case m != nil:
			themes[0][name], themes[1][name] = m[1], m[2]
		case strings.HasPrefix(value, "#"), strings.HasPrefix(value, "rgb"):
			t.Errorf("--%s is %s in both themes; a color token is a light-dark(<light>, <dark>) pair", name, value)
		default:
			themes[0][name], themes[1][name] = value, value
		}
	}

	if t.Failed() {
		t.FailNow()
	}

	return themes
}

// TestTextTonesMeetContrast: every tone text is drawn in — the greys and the status colors a cell or a verdict is written in — is readable on every ground a page puts it on, in BOTH themes, at WCAG AA's 4.5:1 for small text, which is all of this UI's text.
func TestTextTonesMeetContrast(t *testing.T) {
	t.Parallel()

	css, err := assets.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}

	for i, tokens := range themeTokens(t, css) {
		for _, text := range []string{"fg", "dim", "faint", "green", "red", "yellow", "blue", "magenta", "cyan"} {
			for _, ground := range []string{"bg", "panel", "panel2", "code-bg"} {
				ratio := contrast(t, tokens[text], tokens[ground])
				if ratio < 4.5 {
					t.Errorf("%s: --%s on --%s is %.2f:1, under 4.5:1", themeNames[i], text, ground, ratio)
				}
			}
		}
	}

	if regexp.MustCompile(`(?m)(^|[\s;{])color:\s*var\(--faint-line\)`).Match(css) {
		t.Error("--faint-line is drawing text; it is for rules and edges, and fails contrast as a text color")
	}
}

// TestNoColorOutsideTokens: a color written into a rule is one theme's color, so every color lives in the token block and rules reach it through var() and color-mix(). The dark tints this found were each a light-mode bug waiting: a #33452d border is invisible on paper.
func TestNoColorOutsideTokens(t *testing.T) {
	t.Parallel()

	css, err := assets.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}

	body := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(string(css), "")

	start := strings.Index(body, ":root {")
	if start < 0 {
		t.Fatal("no :root token block")
	}

	end := strings.Index(body[start:], "}")
	body = body[:start] + body[start+end+1:]

	literal := regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b|\b(rgba?|hsla?)\(`)
	for _, decls := range regexp.MustCompile(`\{([^{}]*)\}`).FindAllStringSubmatch(body, -1) {
		for _, decl := range strings.Split(decls[1], ";") {
			if literal.MatchString(decl) {
				t.Errorf("a color outside the token block: %s", strings.TrimSpace(decl))
			}
		}
	}
}

func contrast(t *testing.T, a, b string) float64 {
	t.Helper()

	la, lb := luminance(t, a), luminance(t, b)

	return (math.Max(la, lb) + 0.05) / (math.Min(la, lb) + 0.05)
}

func luminance(t *testing.T, hex string) float64 {
	t.Helper()

	if len(hex) != 7 || hex[0] != '#' {
		t.Fatalf("not a #rrggbb color: %q", hex)
	}

	weights := []float64{0.2126, 0.7152, 0.0722}
	total := 0.0

	for i, weight := range weights {
		channel, err := strconv.ParseUint(hex[1+2*i:3+2*i], 16, 8)
		if err != nil {
			t.Fatalf("%q: %v", hex, err)
		}

		v := float64(channel) / 255
		if v <= 0.03928 {
			v /= 12.92
		} else {
			v = math.Pow((v+0.055)/1.055, 2.4)
		}

		total += weight * v
	}

	return total
}

// TestGraphNodeTintsKeepTextReadable: a job node is filled with its status
// color mixed into the ground at --dag-tint. Every text a node draws — its
// name in --fg, its status line in its own color, a running line in
// --yellow — must still clear 4.5:1 on that mix. Red is the tight one: over
// --panel2 even an 8% tint failed, which is why the mix is over --bg.
func TestGraphNodeTintsKeepTextReadable(t *testing.T) {
	t.Parallel()

	css, err := assets.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}

	rules := regexp.MustCompile(`fill:\s*color-mix\(in srgb, var\(--([a-z0-9-]+)\) var\(--dag-tint\), var\(--([a-z0-9-]+)\)\)`).FindAllStringSubmatch(string(css), -1)
	if len(rules) == 0 {
		t.Fatal("no node is tinted with --dag-tint; this test would be checking nothing")
	}

	for i, tokens := range themeTokens(t, css) {
		tint, err := strconv.ParseFloat(strings.TrimSuffix(tokens["dag-tint"], "%"), 64)
		if err != nil {
			t.Fatalf("--dag-tint %q is not a percentage: %v", tokens["dag-tint"], err)
		}

		for _, rule := range rules {
			fill := mixHex(t, tokens[rule[1]], tokens[rule[2]], tint/100)

			for _, text := range []string{"fg", rule[1], "yellow"} {
				if ratio := contrast(t, tokens[text], fill); ratio < 4.5 {
					t.Errorf("%s: --%s on --%s tinted %s%% over --%s is %.2f:1, under 4.5:1", themeNames[i], text, rule[1], tokens["dag-tint"], rule[2], ratio)
				}
			}
		}
	}
}

// mixHex is color-mix(in srgb, a p, b): a straight per-channel blend.
func mixHex(t *testing.T, a, b string, p float64) string {
	t.Helper()

	var channels [3]int

	for i := range channels {
		x, errA := strconv.ParseUint(a[1+2*i:3+2*i], 16, 8)
		y, errB := strconv.ParseUint(b[1+2*i:3+2*i], 16, 8)

		if errA != nil || errB != nil {
			t.Fatalf("cannot mix %q and %q", a, b)
		}

		channels[i] = int(math.Round(float64(x)*p + float64(y)*(1-p)))
	}

	return fmt.Sprintf("#%02x%02x%02x", channels[0], channels[1], channels[2])
}

// TestPausedBarKeepsItsTextReadable: the paused toolbar is tinted with its status color, and two kinds of text sit on the tint itself rather than on a button — the blue "everything is paused" and the dim read-only hints. At 16% over --panel both failed 4.5:1, in dark as well as light, which is why the tint is light and over --bg.
func TestPausedBarKeepsItsTextReadable(t *testing.T) {
	t.Parallel()

	css, err := assets.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}

	rule := regexp.MustCompile(`\.actionbar\.paused \{ background: color-mix\(in srgb, var\(--([a-z0-9-]+)\) (\d+)%, var\(--([a-z0-9-]+)\)\)`).FindStringSubmatch(string(css))
	if rule == nil {
		t.Fatal("the paused toolbar is not tinted with a color-mix this test can read")
	}

	percent, err := strconv.ParseFloat(rule[2], 64)
	if err != nil {
		t.Fatal(err)
	}

	for i, tokens := range themeTokens(t, css) {
		ground := mixHex(t, tokens[rule[1]], tokens[rule[3]], percent/100)

		for _, text := range []string{"fg", "dim", rule[1]} {
			if ratio := contrast(t, tokens[text], ground); ratio < 4.5 {
				t.Errorf("%s: --%s on the paused toolbar (%s%% --%s over --%s) is %.2f:1, under 4.5:1", themeNames[i], text, rule[2], rule[1], rule[3], ratio)
			}
		}
	}
}
