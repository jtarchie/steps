package web

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestTextTonesMeetContrast: every grey text is drawn in is readable on every ground a page puts it on, at WCAG AA's 4.5:1 for small text — which is all of this UI's text.
func TestTextTonesMeetContrast(t *testing.T) {
	t.Parallel()

	css, err := assets.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}

	tokens := map[string]string{}
	for _, m := range regexp.MustCompile(`(?m)^\s*--([a-z0-9-]+):\s*([^;]+);`).FindAllStringSubmatch(string(css), -1) {
		tokens[m[1]] = strings.TrimSpace(m[2])
	}

	resolve := func(name string) string {
		value := tokens[name]
		for strings.HasPrefix(value, "var(--") {
			value = tokens[strings.TrimSuffix(strings.TrimPrefix(value, "var(--"), ")")]
		}

		return value
	}

	for _, text := range []string{"fg", "dim", "faint"} {
		for _, ground := range []string{"bg", "panel", "panel2", "code-bg"} {
			ratio := contrast(t, resolve(text), resolve(ground))
			if ratio < 4.5 {
				t.Errorf("--%s on --%s is %.2f:1, under 4.5:1", text, ground, ratio)
			}
		}
	}

	if regexp.MustCompile(`(?m)(^|[\s;{])color:\s*var\(--faint-line\)`).Match(css) {
		t.Error("--faint-line is drawing text; it is for rules and edges, and fails contrast as a text color")
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

	tokens := map[string]string{}
	for _, m := range regexp.MustCompile(`(?m)^\s*--([a-z0-9-]+):\s*([^;]+);`).FindAllStringSubmatch(string(css), -1) {
		tokens[m[1]] = strings.TrimSpace(m[2])
	}

	tint, err := strconv.ParseFloat(strings.TrimSuffix(tokens["dag-tint"], "%"), 64)
	if err != nil {
		t.Fatalf("--dag-tint %q is not a percentage: %v", tokens["dag-tint"], err)
	}

	rules := regexp.MustCompile(`fill:\s*color-mix\(in srgb, var\(--([a-z0-9-]+)\) var\(--dag-tint\), var\(--([a-z0-9-]+)\)\)`).FindAllStringSubmatch(string(css), -1)
	if len(rules) == 0 {
		t.Fatal("no node is tinted with --dag-tint; this test would be checking nothing")
	}

	for _, rule := range rules {
		fill := mixHex(t, tokens[rule[1]], tokens[rule[2]], tint/100)

		for _, text := range []string{"fg", rule[1], "yellow"} {
			if ratio := contrast(t, tokens[text], fill); ratio < 4.5 {
				t.Errorf("--%s on --%s tinted %s%% over --%s is %.2f:1, under 4.5:1", text, rule[1], tokens["dag-tint"], rule[2], ratio)
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
