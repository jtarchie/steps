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

// TestFocusRingIsSeenOnEveryGround: the ring is how a keyboard reader knows where they are, so it is held to WCAG 1.4.11's 3:1 against every ground it can sit on, in both themes, and to 2px — the 2.4.13 minimum a 1px hairline in --yellow did not reach.
func TestFocusRingIsSeenOnEveryGround(t *testing.T) {
	t.Parallel()

	css, err := assets.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}

	rule := regexp.MustCompile(`(?m)^:focus-visible \{ outline: (\d+)px solid var\(--([a-z0-9-]+)\);`).FindSubmatch(css)
	if rule == nil {
		t.Fatal("no global :focus-visible outline this test can read")
	}

	if width, _ := strconv.Atoi(string(rule[1])); width < 2 {
		t.Errorf("the focus ring is %dpx; 2px is the minimum", width)
	}

	for i, tokens := range themeTokens(t, css) {
		for _, ground := range []string{"bg", "panel", "panel2", "code-bg"} {
			if ratio := contrast(t, tokens[string(rule[2])], tokens[ground]); ratio < 3 {
				t.Errorf("%s: the focus ring --%s on --%s is %.2f:1, under 3:1", themeNames[i], rule[2], ground, ratio)
			}
		}
	}
}

// TestRunHistoryTicksDifferBeyondHue: a job's history strip is a row of small squares, one per run, with no glyph or word beside each — so two statuses drawn as the same shape are told apart by hue alone (WCAG 1.4.1), and red against green is exactly the pair red-green colour blindness loses. Two ticks must differ in SHAPE, or stay at least ΔE2000 10 apart under every simulated deficiency in both themes; 10 is the distance colour-blindness palette tools treat as clearly distinct for small marks, where ~2 is merely perceptible.
func TestRunHistoryTicksDifferBeyondHue(t *testing.T) {
	t.Parallel()

	css, err := assets.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}

	ticksDifferBeyondHue(t, css, "tick", []string{"passed", "failed", "errored", "aborted", "running", "queued"})
}

// TestJobMapTicksDifferBeyondHue: a map row's ticks are the strip's squares for jobs instead of runs, with held and never ran added — held is red like failed, so only its shape tells them apart.
func TestJobMapTicksDifferBeyondHue(t *testing.T) {
	t.Parallel()

	css, err := assets.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}

	ticksDifferBeyondHue(t, css, "mapjob", []string{"passed", "failed", "errored", "held", "aborted", "running", "queued", "none"})
}

func ticksDifferBeyondHue(t *testing.T, css []byte, class string, statuses []string) {
	t.Helper()

	color, shape := tickLooks(t, css, class, statuses)

	for i, tokens := range themeTokens(t, css) {
		for a := range statuses {
			for b := a + 1; b < len(statuses); b++ {
				x, y := statuses[a], statuses[b]
				if shape[x] != shape[y] {
					continue
				}

				for _, vision := range visions {
					if d := deltaE2000(lab(vision.simulate(tokens[color[x]])), lab(vision.simulate(tokens[color[y]]))); d < 10 {
						t.Errorf("%s, %s: a %s tick and a %s tick are the same shape and only ΔE %.1f apart", themeNames[i], vision.name, x, y, d)
					}
				}
			}
		}
	}
}

// tickLooks reads, per status, the color token its .st- class writes text in and the declarations its .<class> rule adds beyond color and motion: its shape.
func tickLooks(t *testing.T, css []byte, class string, statuses []string) (color, shape map[string]string) {
	t.Helper()

	color, shape = map[string]string{}, map[string]string{}

	for _, status := range statuses {
		m := regexp.MustCompile(`(?m)^[^{\n]*\.st-` + status + `\b[^{\n]*\{ color: var\(--([a-z0-9-]+)\); \}`).FindSubmatch(css)
		if m == nil {
			t.Fatalf(".st-%s sets no color this test can read", status)
		}

		color[status] = string(m[1])

		var decls []string
		for _, rule := range regexp.MustCompile(`(?m)^\.`+class+`\.st-`+status+` \{([^}]*)\}`).FindAllSubmatch(css, -1) {
			for _, decl := range strings.Split(string(rule[1]), ";") {
				if decl = strings.TrimSpace(decl); decl != "" && !strings.HasPrefix(decl, "animation") {
					decls = append(decls, decl)
				}
			}
		}

		shape[status] = strings.Join(decls, ";")
	}

	return color, shape
}

type vision struct {
	name     string
	simulate func(hex string) [3]float64
}

// visions simulate dichromacy on linear sRGB with the models DaltonLens rates best (daltonlens.org/opensource-cvd-simulation): Viénot 1999 for protan and deutan, Brettel 1997 for tritan, which Viénot and Machado do not model. Matrices are DaltonLens' precomputed sRGB ones; every row sums to 1, so white stays white.
var visions = []vision{
	{"normal vision", linearRGB},
	{"protanopia", func(hex string) [3]float64 {
		return mul([3][3]float64{{0.11238, 0.88762, 0}, {0.11238, 0.88762, 0}, {0.00401, -0.00401, 1}}, linearRGB(hex))
	}},
	{"deuteranopia", func(hex string) [3]float64 {
		return mul([3][3]float64{{0.29275, 0.70725, 0}, {0.29275, 0.70725, 0}, {-0.02234, 0.02234, 1}}, linearRGB(hex))
	}},
	{"tritanopia", func(hex string) [3]float64 {
		rgb := linearRGB(hex)
		if 0.03901*rgb[0]-0.02788*rgb[1]-0.01113*rgb[2] >= 0 {
			return mul([3][3]float64{{1.01277, 0.13548, -0.14826}, {-0.01243, 0.86812, 0.14431}, {0.07589, 0.80500, 0.11911}}, rgb)
		}

		return mul([3][3]float64{{0.93678, 0.18979, -0.12657}, {0.06154, 0.81526, 0.12320}, {-0.37562, 1.12767, 0.24796}}, rgb)
	}},
}

func linearRGB(hex string) [3]float64 {
	var out [3]float64

	for i := range out {
		channel, _ := strconv.ParseUint(hex[1+2*i:3+2*i], 16, 8)

		v := float64(channel) / 255
		if v <= 0.04045 {
			out[i] = v / 12.92
		} else {
			out[i] = math.Pow((v+0.055)/1.055, 2.4)
		}
	}

	return out
}

func mul(m [3][3]float64, v [3]float64) [3]float64 {
	var out [3]float64
	for i := range out {
		out[i] = math.Min(1, math.Max(0, m[i][0]*v[0]+m[i][1]*v[1]+m[i][2]*v[2]))
	}

	return out
}

// lab is CIE L*a*b* (D65) of a linear sRGB color.
func lab(rgb [3]float64) [3]float64 {
	x := (0.4124*rgb[0] + 0.3576*rgb[1] + 0.1805*rgb[2]) / 0.95047
	y := 0.2126*rgb[0] + 0.7152*rgb[1] + 0.0722*rgb[2]
	z := (0.0193*rgb[0] + 0.1192*rgb[1] + 0.9505*rgb[2]) / 1.08883

	f := func(t float64) float64 {
		if t > 216.0/24389 {
			return math.Cbrt(t)
		}

		return (24389.0/27*t + 16) / 116
	}

	return [3]float64{116*f(y) - 16, 500 * (f(x) - f(y)), 200 * (f(y) - f(z))}
}

// deltaE2000 is CIEDE2000 (Sharma, Wu and Dalal 2005).
func deltaE2000(p, q [3]float64) float64 {
	const rad = math.Pi / 180

	c1, c2 := math.Hypot(p[1], p[2]), math.Hypot(q[1], q[2])
	cBar7 := math.Pow((c1+c2)/2, 7)
	g := 0.5 * (1 - math.Sqrt(cBar7/(cBar7+math.Pow(25, 7))))
	a1, a2 := (1+g)*p[1], (1+g)*q[1]
	c1p, c2p := math.Hypot(a1, p[2]), math.Hypot(a2, q[2])

	hue := func(b, a float64) float64 {
		h := math.Atan2(b, a) / rad
		if h < 0 {
			h += 360
		}

		return h
	}

	h1, h2 := hue(p[2], a1), hue(q[2], a2)

	dh := h2 - h1

	switch {
	case c1p*c2p == 0:
		dh = 0
	case dh > 180:
		dh -= 360
	case dh < -180:
		dh += 360
	}

	dL, dC, dH := q[0]-p[0], c2p-c1p, 2*math.Sqrt(c1p*c2p)*math.Sin(dh*rad/2)

	lBar, cBarP, hBar := (p[0]+q[0])/2, (c1p+c2p)/2, h1+h2

	if c1p*c2p != 0 {
		if math.Abs(h1-h2) > 180 {
			hBar = (h1 + h2 + 360) / 2
		} else {
			hBar = (h1 + h2) / 2
		}
	}

	tt := 1 - 0.17*math.Cos((hBar-30)*rad) + 0.24*math.Cos(2*hBar*rad) + 0.32*math.Cos((3*hBar+6)*rad) - 0.2*math.Cos((4*hBar-63)*rad)
	cP7 := math.Pow(cBarP, 7)
	rc := 2 * math.Sqrt(cP7/(cP7+math.Pow(25, 7)))
	sl := 1 + 0.015*(lBar-50)*(lBar-50)/math.Sqrt(20+(lBar-50)*(lBar-50))
	sc, sh := 1+0.045*cBarP, 1+0.015*cBarP*tt
	dTheta := (hBar - 275) / 25
	rt := -math.Sin(2*30*math.Exp(-dTheta*dTheta)*rad) * rc

	l, c, h := dL/sl, dC/sc, dH/sh

	return math.Sqrt(l*l + c*c + h*h + rt*c*h)
}

// TestDeltaE2000MatchesSharmaTable pins the metric to rows of the published CIEDE2000 test data, so a typo in it cannot quietly loosen the tick check above.
func TestDeltaE2000MatchesSharmaTable(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		p, q [3]float64
		want float64
	}{
		{[3]float64{50, 2.6772, -79.7751}, [3]float64{50, 0, -82.7485}, 2.0425},
		{[3]float64{50, 2.5, 0}, [3]float64{73, 25, -18}, 27.1492},
		{[3]float64{60.2574, -34.0099, 36.2677}, [3]float64{60.4626, -34.1751, 39.4387}, 1.2644},
		{[3]float64{22.7233, 20.0904, -46.6940}, [3]float64{23.0331, 14.9730, -42.5619}, 2.0373},
	} {
		if got := deltaE2000(row.p, row.q); math.Abs(got-row.want) > 1e-4 {
			t.Errorf("ΔE2000(%v, %v) = %.4f, want %.4f", row.p, row.q, got, row.want)
		}
	}
}

// TestThePaletteIsColorBlindSafe: the six hues are told apart by people with every common color-vision deficiency, not only by people without one. Every pair stays ΔE2000 10 apart under simulated protanopia, deuteranopia and tritanopia, in both themes — the threshold at which Adobe's color-blindness checker (color.adobe.com/create/color-accessibility) stopped flagging the pairs it flagged in the old palette, so this test and that tool agree. A new hue joins the list, or says here why it does not.
func TestThePaletteIsColorBlindSafe(t *testing.T) {
	t.Parallel()

	css, err := assets.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}

	hues := []string{"green", "red", "yellow", "blue", "magenta", "cyan"}

	for i, tokens := range themeTokens(t, css) {
		for a := range hues {
			for b := a + 1; b < len(hues); b++ {
				for _, vision := range visions {
					if d := deltaE2000(lab(vision.simulate(tokens[hues[a]])), lab(vision.simulate(tokens[hues[b]]))); d < 10 {
						t.Errorf("%s, %s: --%s %s and --%s %s are only ΔE %.1f apart", themeNames[i], vision.name, hues[a], tokens[hues[a]], hues[b], tokens[hues[b]], d)
					}
				}
			}
		}
	}
}

// TestTheFaviconDrawsInThePalette: the tab icon's dots are drawn server-side as SVG, so they hold their own copies of the dark theme's colors; a palette change that skipped them shipped a favicon in colors the page no longer uses.
func TestTheFaviconDrawsInThePalette(t *testing.T) {
	t.Parallel()

	css, err := assets.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}

	dark := themeTokens(t, css)[1]

	for token, drawn := range map[string]string{"bg": colorBg, "dim": colorDim, "faint-line": colorFaint, "green": colorGreen, "red": colorRed, "yellow": colorYellow, "blue": colorBlue} {
		if dark[token] != drawn {
			t.Errorf("the favicon draws --%s as %s; the dark theme's is %s", token, drawn, dark[token])
		}
	}
}
