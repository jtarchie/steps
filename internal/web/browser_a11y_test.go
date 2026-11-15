package web

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
)

// The browser tier of the accessibility checks: what only a rendered page can say — computed contrast over real layout, focus order, what a script does on a keypress. It runs whenever a Chrome or Chromium is on the machine (CHROME_PATH names one explicitly) and skips cleanly when none is, the way the docker-backed tests do. axe-core is vendored in testdata/axe, unmodified, under its MPL-2.0 licence beside it.

func chromeOrSkip(t *testing.T) string {
	t.Helper()

	if path := os.Getenv("CHROME_PATH"); path != "" {
		return path
	}

	for _, path := range []string{
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Chromium.app/Contents/MacOS/Chromium",
	} {
		_, err := os.Stat(path)
		if err == nil {
			return path
		}
	}

	for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser"} {
		path, err := exec.LookPath(name)
		if err == nil {
			return path
		}
	}

	t.Skip("no Chrome or Chromium found; set CHROME_PATH to run the browser accessibility checks")

	return ""
}

func browser(t *testing.T) context.Context {
	t.Helper()

	exe := chromeOrSkip(t)

	// The defaults are what make it headless, with no first-run or default-browser prompt; NewExecAllocator does not apply them itself, and without them every run opened a visible Chrome asking to be the default browser.
	alloc, cancelAlloc := chromedp.NewExecAllocator(context.Background(), append(chromedp.DefaultExecAllocatorOptions[:], chromedp.ExecPath(exe), chromedp.WindowSize(1280, 900))...)
	ctx, cancelCtx := chromedp.NewContext(alloc)
	ctx, cancelTimeout := context.WithTimeout(ctx, 3*time.Minute)

	t.Cleanup(func() {
		cancelTimeout()
		cancelCtx()
		cancelAlloc()
	})

	err := chromedp.Do(ctx)
	if err != nil {
		t.Skipf("Chrome at %s did not start: %v", exe, err)
	}

	return ctx
}

// a11yPages is a daemon with something on every page: a passed run and a failed one with steps, an approval and a question waiting, and a second pipeline so the root is the overview rather than a redirect.
func a11yPages(t *testing.T) (base string, pages []string) {
	t.Helper()

	server, pipelines := lanePipelines(t, nil, "demo", twoJobs, "other", oneManualJob)
	demo := pipelines[0]

	for _, run := range []struct{ id, status string }{{"run-ok", "succeeded"}, {"run-bad", "failed"}} {
		err := demo.Store.StartRun(t.Context(), store.NewRun{ID: run.id, JobName: "build", Workspace: "/tmp/ws"})
		if err != nil {
			t.Fatal(err)
		}

		appendEvents(t, demo.Store, run.id, []store.RunEventRow{
			{Type: events.TypeJobStarted, StepIndex: -1},
			{Type: events.TypeStepStarted, StepIndex: 0, StepName: "compile", StepKind: "task"},
			{Type: events.TypeStepOutput, StepIndex: 0, StepName: "compile", StepKind: "task", Text: "building\nhttps://example.com/" + strings.Repeat("unbroken", 60) + "\n"},
			{Type: events.TypeStepFinished, StepIndex: 0, StepName: "compile", StepKind: "task", Status: run.status, Hash: "beef7654321", DurationMS: 42},
		})

		err = demo.Store.FinishRun(t.Context(), run.id, store.RunStatus(run.status))
		if err != nil {
			t.Fatal(err)
		}
	}

	_, err := demo.Store.RequestApproval(t.Context(), "build", "ship it?")
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = demo.Store.AskQuestion(t.Context(), store.Question{RunID: "run-ok", JobName: "build", AgentName: "review", Question: "which branch?"})
	if err != nil {
		t.Fatal(err)
	}

	listener := httptest.NewServer(server.Handler())
	t.Cleanup(listener.Close)

	return listener.URL, []string{
		"/",
		"/p/demo",
		"/p/demo/runs",
		"/p/demo/runs/run-ok",
		"/p/demo/runs/run-bad",
		"/p/demo/jobs/build/detail",
		"/p/demo/resources",
		"/p/demo/approvals",
		"/p/demo/questions",
		"/docs/resources.md",
		"/nosuch",
	}
}

const runAxe = `axe.run(document, {
  runOnly: {type: 'tag', values: ['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22aa', 'best-practice']},
  rules: {'target-size': {enabled: true}}
}).then(r => r.violations.flatMap(v => v.nodes
  // A graph node's spoken label reads "passed 2 minutes ago" where the box draws "2m": the visible name leads the label, and the status words are spelled out for speech rather than abbreviated (WCAG 2.5.3 asks for the visible LABEL, which is the job's name).
  .filter(n => !(v.id === 'label-content-name-mismatch' && n.html.startsWith('<a class="dagnode')))
  .map(n => v.id + ' (' + v.impact + '): ' + n.target.join(' ') + ' — ' + (n.failureSummary || '').split('\n').slice(1).join(' ').trim())))`

func media(scheme string, forced bool) chromedp.Action[chromedp.Void] {
	features := []*emulation.MediaFeature{{Name: "prefers-color-scheme", Value: scheme}, {Name: "prefers-reduced-motion", Value: "reduce"}}
	if forced {
		features = append(features, &emulation.MediaFeature{Name: "forced-colors", Value: "active"})
	}

	return func(ctx context.Context, _ *chromedp.Target) (chromedp.Void, error) {
		_, err := chromedp.Call(ctx, emulation.SetEmulatedMedia, emulation.SetEmulatedMediaParams{Features: features})
		if err != nil {
			return chromedp.Void{}, fmt.Errorf("emulate %s media: %w", scheme, err)
		}

		return chromedp.Void{}, nil
	}
}

// TestEveryPagePassesAxe: every page, in light, dark and forced colors, with the header's menus and the jump palette opened too, has no axe-core violation at WCAG 2.2 AA plus axe's best practices. A rule is waived only by name in runAxe, with the reason beside it.
func TestEveryPagePassesAxe(t *testing.T) {
	ctx := browser(t)
	base, pages := a11yPages(t)

	axeSrc, err := os.ReadFile("testdata/axe/axe.min.js")
	if err != nil {
		t.Fatal(err)
	}

	check := func(label string, actions ...chromedp.Action[chromedp.Void]) {
		t.Helper()

		// Two frames first: app.js marks overflowing boxes from a ResizeObserver, whose callback lands before the next paint, not before load.
		settle := chromedp.Evaluate[chromedp.Void](`new Promise(done => requestAnimationFrame(() => requestAnimationFrame(done)))`, chromedp.EvalAwaitPromise)

		err := chromedp.Do(ctx, append(actions, settle, chromedp.Evaluate[chromedp.Void](string(axeSrc)))...)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}

		found, err := chromedp.Run(ctx, chromedp.Evaluate[[]string](runAxe, chromedp.EvalAwaitPromise))
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}

		for _, violation := range found {
			t.Errorf("%s: %s", label, violation)
		}
	}

	for _, mode := range []struct {
		name   string
		scheme string
		forced bool
	}{{"light", "light", false}, {"dark", "dark", false}, {"forced colors", "dark", true}} {
		for _, path := range pages {
			check(path+" ["+mode.name+"]", media(mode.scheme, mode.forced), chromedp.Navigate(base+path))
		}
	}

	check("/p/demo with the switcher open", media("light", false), chromedp.Navigate(base+"/p/demo"), chromedp.Click(chromedp.CSS("#pipebtn")))
	check("/p/demo with the palette open", media("light", false), chromedp.Navigate(base+"/p/demo"), chromedp.Click(chromedp.CSS("#jumpbtn")), chromedp.KeyEvent("b"), chromedp.Evaluate[chromedp.Void](`new Promise(done => setTimeout(done, 400))`, chromedp.EvalAwaitPromise))
}

type tabStop struct {
	Index    int     `json:"index"`
	Name     string  `json:"name"`
	Ring     bool    `json:"ring"`
	Skip     bool    `json:"skip"`
	Hidden   bool    `json:"hidden"`
	Obscured bool    `json:"obscured"`
	Width    float64 `json:"width"`
}

func (stop *tabStop) check(t *testing.T, path string) {
	t.Helper()

	if !stop.Ring && !stop.Hidden {
		t.Errorf("%s: %q takes focus without a visible ring (outline %gpx)", path, stop.Name, stop.Width)
	}

	if stop.Obscured {
		t.Errorf("%s: %q is focused under the sticky header", path, stop.Name)
	}
}

// readStop names the focused element by the order it was FIRST reached, so a repeat is a cycle and not two links with the same text.
const readStop = `(() => {
  const e = document.activeElement;
  if (!e || e === document.body || e === document.documentElement) return null;
  window.__stops = window.__stops || new Map();
  if (!window.__stops.has(e)) window.__stops.set(e, window.__stops.size + 1);
  const cs = getComputedStyle(e), r = e.getBoundingClientRect();
  const ring = (cs.outlineStyle !== 'none' && parseFloat(cs.outlineWidth) >= 2) || cs.boxShadow !== 'none' ||
    [...e.querySelectorAll('rect')].some(x => parseFloat(getComputedStyle(x).strokeWidth) >= 2);
  const header = document.querySelector('body > header');
  return {
    index: window.__stops.get(e),
    name: (e.getAttribute('aria-label') || e.innerText || e.title || e.tagName).trim().slice(0, 50),
    ring, skip: e.getAttribute('href') === '#main',
    hidden: r.width === 0 && r.height === 0,
    obscured: (() => { const x = r.left + r.width / 2, y = r.top + r.height / 2; if (y < 0 || y > innerHeight) return false; const top = document.elementFromPoint(x, y); return !!top && !e.contains(top) && !top.contains(e) && !!header && header.contains(top); })(),
    width: parseFloat(cs.outlineWidth)
  };
})()`

// TestTheKeyboardReachesEverythingVisibly walks each page with Tab: the first stop skips to the content (2.4.1), every stop shows a ring at least 2px wide (2.4.7, 2.4.13), none sits under the sticky header (2.4.11), and the walk comes back around rather than getting stuck (2.1.2).
func TestTheKeyboardReachesEverythingVisibly(t *testing.T) {
	ctx := browser(t)
	base, pages := a11yPages(t)

	for _, path := range pages {
		walkTabs(ctx, t, base, path)
	}
}

func walkTabs(ctx context.Context, t *testing.T, base, path string) {
	t.Helper()

	count := must(ctx, t, chromedp.Evaluate[int](`document.querySelectorAll('a[href], button, input, select, textarea, summary, [tabindex]:not([tabindex="-1"])').length`), media("light", false), chromedp.Navigate(base+path))

	for press := range count + 5 {
		stop := must(ctx, t, chromedp.Evaluate[*tabStop](readStop), chromedp.KeyEvent(kb.Tab))
		if stop == nil || (press > 0 && stop.Index == 1) {
			return
		}

		if press == 0 && !stop.Skip {
			t.Errorf("%s: the first Tab lands on %q, not the skip link", path, stop.Name)
		}

		if stop.Index <= press {
			t.Errorf("%s: Tab returned to %q mid-walk — a trap or a loop", path, stop.Name)

			return
		}

		stop.check(t, path)
	}

	t.Errorf("%s: %d Tabs never came back around", path, count+5)
}

// must runs the steps, then reads what the action returns, and fails the test on any error.
func must[T any](ctx context.Context, t *testing.T, read chromedp.Action[T], steps ...chromedp.Action[chromedp.Void]) T {
	t.Helper()

	err := chromedp.Do(ctx, steps...)
	if err != nil {
		t.Fatal(err)
	}

	v, err := chromedp.Run(ctx, read)
	if err != nil {
		t.Fatal(err)
	}

	return v
}

// TestTheRunPageScriptKeepsItsPromises: what app.js owes a keyboard and a screen reader on the run page — a toggle that says whether its row is open, single-key shortcuts that stop when switched off and stay off across a reload (2.1.4), and a wide output box that can be focused to scroll it (2.1.1).
func TestTheRunPageScriptKeepsItsPromises(t *testing.T) {
	ctx := browser(t)
	base, _ := a11yPages(t)

	str := chromedp.Evaluate[string]
	toggle := `document.querySelector('button.steptoggle')`

	before := must(ctx, t, str(toggle+`.getAttribute('aria-expanded')`), media("light", false), chromedp.Navigate(base+"/p/demo/runs/run-ok"))
	after := must(ctx, t, str(toggle+`.getAttribute('aria-expanded')`), chromedp.Click(chromedp.CSS("button.steptoggle")))
	expanded := must(ctx, t, str(`String(`+toggle+`.closest('.step').classList.contains('open'))`))
	focusableWide := must(ctx, t, chromedp.Evaluate[bool](`new Promise(done => requestAnimationFrame(() => requestAnimationFrame(() => done([...document.querySelectorAll('main pre')].some(p => p.scrollWidth > p.clientWidth && p.tabIndex === 0)))))`, chromedp.EvalAwaitPromise))
	openAfterC := must(ctx, t, str(`String(document.querySelectorAll('.step.open').length)`), chromedp.Click(chromedp.CSS(".keystoggle")), chromedp.Evaluate[chromedp.Void](`document.activeElement.blur()`), chromedp.KeyEvent("c"))
	keysAfterReload := must(ctx, t, str(`document.documentElement.dataset.keys || ''`), chromedp.Reload())

	if before == after || after != expanded {
		t.Errorf("the step toggle read aria-expanded=%s, then %s after a click that left the row open=%s", before, after, expanded)
	}

	if !focusableWide {
		t.Error("an output box wider than its column cannot take focus, so a keyboard cannot scroll it")
	}

	if expanded == "true" && openAfterC == "0" {
		t.Error("c still collapsed every row after the keys were switched off")
	}

	if keysAfterReload != "off" {
		t.Errorf("switching the keys off did not survive a reload (data-keys=%q)", keysAfterReload)
	}
}

// TestThePaletteSaysWhichResultEnterOpens: focus stays in the palette's text box, so the only way a screen reader learns which result the arrows are on is aria-activedescendant naming a drawn option.
func TestThePaletteSaysWhichResultEnterOpens(t *testing.T) {
	ctx := browser(t)
	base, _ := a11yPages(t)

	named := must(ctx, t, chromedp.Evaluate[string](`new Promise(done => setTimeout(() => {
	  const input = document.getElementById('palette-input');
	  const id = input.getAttribute('aria-activedescendant');
	  const option = id && document.getElementById(id);
	  done(input.getAttribute('aria-expanded') + ' ' + (option ? option.getAttribute('role') + ' ' + option.getAttribute('aria-selected') + ' ' + option.textContent : 'none'));
	}, 500))`, chromedp.EvalAwaitPromise), media("light", false), chromedp.Navigate(base+"/p/demo"), chromedp.Click(chromedp.CSS("#jumpbtn")), chromedp.KeyEvent("build"))

	if !strings.HasPrefix(named, "true option true") || !strings.Contains(named, "build") {
		t.Errorf("after typing build, the palette reads %q; want it expanded, naming a selected option for build", named)
	}
}
