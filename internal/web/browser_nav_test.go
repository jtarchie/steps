package web

import (
	"context"
	"encoding/base64"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

// navLayout is what a reader sees of the header: how tall its pinned part is, which way to the sections is on screen, and what the menu button counts.
type navLayout struct {
	Header      float64 `json:"header"`
	TabsShown   int     `json:"tabsShown"`
	TabsTotal   int     `json:"tabsTotal"`
	MenuShown   bool    `json:"menuShown"`
	MenuSays    string  `json:"menuSays"`
	PausedShown bool    `json:"pausedShown"`
	PausedRule  bool    `json:"pausedRule"`
	Overflow    float64 `json:"overflow"`
}

const readNav = `(() => {
  const shown = e => !!e && e.getClientRects().length > 0 && getComputedStyle(e).visibility !== 'hidden';
  const tabs = [...document.querySelectorAll('.tabs .tab')];
  const menu = document.querySelector('#navbtn');
  return {
    header: document.querySelector('body > header').getBoundingClientRect().height,
    tabsShown: tabs.filter(shown).length,
    tabsTotal: tabs.length,
    menuShown: shown(menu),
    menuSays: menu ? menu.textContent.replace(/\s+/g, ' ').trim() : '',
    pausedShown: shown(document.querySelector('body > header .actionbar.paused')),
    pausedRule: (() => {
      const probe = document.createElement('i');
      probe.style.color = 'var(--blue)';
      document.body.append(probe);
      const blue = getComputedStyle(probe).color;
      probe.remove();
      const bar = getComputedStyle(document.querySelector('.statusline'));
      return parseFloat(bar.borderBottomWidth) >= 2 && bar.borderBottomColor === blue;
    })(),
    overflow: document.documentElement.scrollWidth - innerWidth,
  };
})()`

// navServer serves a paused, read-only pipeline with one approval waiting: a bar that only says something, and one tab badge.
func navServer(t *testing.T) string {
	t.Helper()

	server, pipeline := testPipeline(t)

	_, err := pipeline.Store.RequestApproval(t.Context(), "deploy", "ship it?")
	if err != nil {
		t.Fatal(err)
	}

	err = pipeline.Store.Pause(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	listener := httptest.NewServer(server.Handler())
	t.Cleanup(listener.Close)

	return listener.URL
}

func onPhone() chromedp.Action[chromedp.Void] {
	return chromedp.EmulateViewport(375, 812, chromedp.EmulateMobile, chromedp.EmulateTouch)
}

// On a phone the header is one row — brand, pipeline, a menu named for the section you are on, the palette — instead of four that took a fifth of the screen. The menu carries the count of work waiting on you, so folding the tabs does not hide the badges that are the reason to look at the bar.
func TestAPhoneFoldsTheHeaderIntoOneRow(t *testing.T) {
	ctx := browser(t)
	base := navServer(t)

	closed := must(ctx, t, chromedp.Evaluate[navLayout](readNav), onPhone(), media("dark", false), chromedp.Navigate(base+"/p/demo/runs"))

	if closed.Header > 50 {
		t.Errorf("the phone header is %.0fpx tall, want one row (≤50px)", closed.Header)
	}

	if closed.TabsShown != 0 || !closed.MenuShown {
		t.Errorf("closed, the phone header shows %d tabs and the menu button = %v; want the menu alone", closed.TabsShown, closed.MenuShown)
	}

	if closed.MenuSays != "runs ●1 Sections" {
		t.Errorf("the menu button says %q; want the section it is on and the waiting approval", closed.MenuSays)
	}

	// The bar that said "paused" folded into the menu; the rule it leaves is how a closed header still says so.
	if !closed.PausedRule {
		t.Error("closed, the phone header does not mark the pipeline paused")
	}

	if closed.Overflow > 0 {
		t.Errorf("the phone page scrolls sideways by %.0fpx", closed.Overflow)
	}
}

// Opened, the phone's menu holds every tab and the bar that says the pipeline is paused.
func TestAPhonesMenuHoldsTheTabsAndThePause(t *testing.T) {
	ctx := browser(t)
	base := navServer(t)

	opened := must(ctx, t, chromedp.Evaluate[navLayout](readNav), onPhone(), media("dark", false), chromedp.Navigate(base+"/p/demo/runs"), chromedp.Click(chromedp.CSS("#navbtn")))

	if opened.TabsShown != opened.TabsTotal || opened.TabsTotal == 0 {
		t.Errorf("open, the menu shows %d of %d tabs", opened.TabsShown, opened.TabsTotal)
	}

	if !opened.PausedShown {
		t.Error("open, the menu does not say the pipeline is paused")
	}
}

// A wide screen keeps its tabs: the menu is a phone's stand-in for them.
func TestAWideScreenKeepsItsTabs(t *testing.T) {
	ctx := browser(t)
	base := navServer(t)

	wide := must(ctx, t, chromedp.Evaluate[navLayout](readNav), chromedp.EmulateViewport(1280, 800), media("dark", false), chromedp.Navigate(base+"/p/demo/runs"))

	if wide.MenuShown || wide.TabsShown != wide.TabsTotal {
		t.Errorf("on a wide screen the menu button = %v and %d of %d tabs show; want the tabs as they were", wide.MenuShown, wide.TabsShown, wide.TabsTotal)
	}
}

// A read-only pipeline that is not paused has a bar with nothing in it, and the open menu does not draw it as an empty strip.
func TestAPhonesMenuDrawsNoEmptyBar(t *testing.T) {
	ctx := browser(t)
	server, _ := testPipeline(t)

	listener := httptest.NewServer(server.Handler())
	t.Cleanup(listener.Close)

	shown := must(ctx, t, chromedp.Evaluate[bool](`(() => {
  const bar = document.querySelector('body > header .actionbar');
  return !!bar && bar.getClientRects().length > 0;
})()`), onPhone(), media("dark", false), chromedp.Navigate(listener.URL+"/p/demo/runs"), chromedp.Click(chromedp.CSS("#navbtn")))

	if shown {
		t.Error("the open phone menu draws an empty action bar")
	}
}

func onLandscapePhone() chromedp.Action[chromedp.Void] {
	return chromedp.EmulateViewport(812, 375, chromedp.EmulateMobile, chromedp.EmulateTouch, chromedp.EmulateLandscape)
}

// A phone held sideways is wide but short, and a header keyed on width alone gave it the desktop bar: 125px pinned on a 375px screen, a third of it.
func TestALandscapePhoneFoldsTheHeaderToo(t *testing.T) {
	ctx := browser(t)
	base := navServer(t)

	got := must(ctx, t, chromedp.Evaluate[navLayout](readNav), onLandscapePhone(), media("dark", false), chromedp.Navigate(base+"/p/demo/runs"))

	if got.Header > 50 || !got.MenuShown || got.TabsShown != 0 {
		t.Errorf("sideways, the header is %.0fpx with the menu button = %v and %d tabs showing; want the phone's one row", got.Header, got.MenuShown, got.TabsShown)
	}
}

// Open on a short screen, the menu is taller than the viewport; the header is pinned, so unless it scrolls itself its last rows are out of reach for good.
func TestAShortScreenReachesTheWholeMenu(t *testing.T) {
	ctx := browser(t)
	base := navServer(t)

	reach := must(ctx, t, chromedp.Evaluate[string](`(() => {
  const last = [...document.querySelectorAll('body > header .themebtn, body > header .actionbar')].filter(e => e.getClientRects().length).pop();
  if (!last) return 'nothing open';
  last.scrollIntoView({block: 'end'});
  const r = last.getBoundingClientRect();
  return r.bottom <= innerHeight + 1 && r.top >= 0 ? 'ok' : 'its last row sits at ' + Math.round(r.top) + '-' + Math.round(r.bottom) + 'px of a ' + innerHeight + 'px screen';
})()`), chromedp.EmulateViewport(812, 300, chromedp.EmulateMobile, chromedp.EmulateTouch, chromedp.EmulateLandscape), media("dark", false), chromedp.Navigate(base+"/p/demo/runs"), chromedp.Click(chromedp.CSS("#navbtn")))

	if reach != "ok" {
		t.Errorf("open on a short screen, the menu cannot be scrolled to its end: %s", reach)
	}
}

// Forced colors drop a box-shadow, which was the only mark on the menu's current tab; the jump button is a thumb's width like the rest of the row.
func TestAPhonesMenuSurvivesForcedColorsAndThumbs(t *testing.T) {
	ctx := browser(t)
	base := navServer(t)

	type marks struct {
		Weight int     `json:"weight"`
		Jump   float64 `json:"jump"`
	}

	got := must(ctx, t, chromedp.Evaluate[marks](`(() => ({
  weight: parseInt(getComputedStyle(document.querySelector('.tabs .tab[aria-current="page"]')).fontWeight, 10),
  jump: document.querySelector('#jumpbtn').getBoundingClientRect().width,
}))()`), onPhone(), media("dark", true), chromedp.Navigate(base+"/p/demo/runs"), chromedp.Click(chromedp.CSS("#navbtn")))

	if got.Weight < 600 {
		t.Errorf("in forced colors the menu's current tab is weight %d, the same as the rest; want it bold", got.Weight)
	}

	if got.Jump < 44 {
		t.Errorf("the jump button is %.0fpx wide, under a 44px thumb", got.Jump)
	}
}

// The account menu sits inside the phone's menu, and the two share one open record: its button closed the menu it lived in, taking itself and sign-out off the screen. On a phone the account's links are rows of the menu instead.
func TestAPhonesMenuReachesTheAccount(t *testing.T) {
	ctx := browser(t)
	_, pipeline := testPipeline(t)

	server, err := New([]*Pipeline{pipeline}, nil, WithBasicAuth("ops", "pw"))
	if err != nil {
		t.Fatal(err)
	}

	listener := httptest.NewServer(server.Handler())
	t.Cleanup(listener.Close)

	var signIn chromedp.Action[chromedp.Void] = func(ctx context.Context, _ *chromedp.Target) (chromedp.Void, error) {
		_, err := chromedp.Call(ctx, network.Enable, network.EnableParams{})
		if err != nil {
			return chromedp.Void{}, fmt.Errorf("enable network: %w", err)
		}

		_, err = chromedp.Call(ctx, network.SetExtraHTTPHeaders, network.SetExtraHTTPHeadersParams{Headers: network.Headers{"Authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte("ops:pw"))}})
		if err != nil {
			return chromedp.Void{}, fmt.Errorf("sign in: %w", err)
		}

		return chromedp.Void{}, nil
	}

	shown := must(ctx, t, chromedp.Evaluate[bool](`(() => {
  const link = document.querySelector('#usermenu a[href="/activity"]');
  return !!link && link.getClientRects().length > 0;
})()`), signIn, onPhone(), media("dark", false), chromedp.Navigate(listener.URL+"/p/demo/runs"), chromedp.Click(chromedp.CSS("#navbtn")))

	if !shown {
		t.Error("open, the phone menu does not reach the account's links")
	}
}

// The root's paused bar sets "everything is paused" beside Resume all, and the words sat at the top of a box stretched to the button's height, a few pixels above the button's own label.
func TestThePausedBarCentersItsWordsOnItsButton(t *testing.T) {
	ctx := browser(t)

	_, pipelines := testPipelines(t, "alpha", "beta")

	server, err := New(pipelines, stubRunner{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	listener := httptest.NewServer(server.Handler())
	t.Cleanup(listener.Close)

	if code, _ := postForm(t, server, "/pause-all", nil); code != http.StatusSeeOther {
		t.Fatalf("pause all answered %d, want 303", code)
	}

	const offset = `(() => {
  const mid = e => { const range = document.createRange(); range.selectNodeContents(e); const r = range.getBoundingClientRect(); return r.top + r.height / 2; };
  const bar = document.querySelector('.actionbar.paused');
  return mid(bar.querySelector('.st-paused')) - mid(bar.querySelector('button'));
})()`

	off := must(ctx, t, chromedp.Evaluate[float64](offset), chromedp.EmulateViewport(1280, 800), media("dark", false), chromedp.Navigate(listener.URL+"/"))

	if math.Abs(off) > 1 {
		t.Errorf("\"everything is paused\" sits %.1fpx off Resume all's middle, want them centered on one line", off)
	}
}

// TestTheKeysWalkIntoAJobMap: h and l walk a row's job cells, and a map row's ticks are its jobs, so the walk goes on past the named cells into them; a tick the keys skip is a job a keyboard reader cannot open from the root.
func TestTheKeysWalkIntoAJobMap(t *testing.T) {
	ctx := browser(t)

	server, pipelines := lanePipelines(t, nil, "a-wide", manyJobs(8), "b-other", oneManualJob)
	seedRun(t, pipelines[0], "run-6", "j6", "failed")

	listener := httptest.NewServer(server.Handler())
	t.Cleanup(listener.Close)

	focused := chromedp.Evaluate[string](`document.activeElement.getAttribute('aria-label') || ''`)
	got := must(ctx, t, focused, media("dark", false), chromedp.Navigate(listener.URL+"/"), chromedp.KeyEvent("j"), chromedp.KeyEvent("l"), chromedp.KeyEvent("l"), chromedp.KeyEvent("l"))

	if got != "j2, never ran" {
		t.Errorf("j then l three times lands on %q, want the map's second tick", got)
	}
}

// TestAWallThatStopsHearingGoesGrey: a wall whose daemon went away kept painting the last answer it had, so a broken build could sit green on the TV all afternoon. Once the server's render stamp stops changing the wall greys and says since when.
func TestAWallThatStopsHearingGoesGrey(t *testing.T) {
	ctx := browser(t)

	server, _ := testPipelines(t, "app", "infra")
	listener := httptest.NewServer(server.Handler())

	stale := chromedp.Evaluate[bool](`document.documentElement.classList.contains('stale') && getComputedStyle(document.querySelector('.wallstale')).display !== 'none'`)

	// 4s against a 2.5s poll, watched for 7s: a wall that heard nothing since it loaded is stale by then, one whose polls land is not.
	alive := must(ctx, t, stale, media("dark", false), chromedp.Navigate(listener.URL+"/wall"), chromedp.Evaluate[chromedp.Void](`document.documentElement.dataset.staleAfter = '4000'`), chromedp.Evaluate[chromedp.Void](`new Promise(done => setTimeout(done, 7000))`, chromedp.EvalAwaitPromise))
	if alive {
		t.Fatal("a wall whose polls are landing says it is not updating")
	}

	listener.Close()

	gone := must(ctx, t, stale, chromedp.Evaluate[chromedp.Void](`document.documentElement.dataset.staleAfter = '1000'`), chromedp.Evaluate[chromedp.Void](`new Promise(done => setTimeout(done, 3000))`, chromedp.EvalAwaitPromise))
	if !gone {
		t.Error("a wall whose server went away still claims to be current")
	}

	// "last heard" counts on by the second: at a 30-second tick it read "5s ago" for half a minute after the daemon died.
	heard := must(ctx, t, chromedp.Evaluate[string](`document.querySelector('.wallstale time').textContent`))
	if !strings.HasSuffix(heard, "s ago") || strings.HasPrefix(heard, "0") {
		t.Errorf("a stale wall says it last heard %q; want the seconds since, counting", heard)
	}
}

// TestAWallAnsweredWithAnErrorGoesGreyAndComesBack: a proxy's 502 while the daemon restarts was swapped in over #wall, which took the signs, the poll and the stale check with them, so the screen sat blank until somebody walked over and reloaded it.
func TestAWallAnsweredWithAnErrorGoesGreyAndComesBack(t *testing.T) {
	ctx := browser(t)

	server, _ := testPipelines(t, "app", "infra")

	var failing atomic.Bool

	handler := server.Handler()
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failing.Load() {
			http.Error(w, "<html><body>bad gateway</body></html>", http.StatusBadGateway)

			return
		}

		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(listener.Close)

	read := chromedp.Evaluate[string](`(document.getElementById('wall') ? 'wall' : 'gone') + ' ' + (document.documentElement.classList.contains('stale') ? 'stale' : 'fresh')`)

	if loaded := must(ctx, t, read, chromedp.Navigate(listener.URL+"/wall"), chromedp.Evaluate[chromedp.Void](`document.documentElement.dataset.staleAfter = '4000'`)); loaded != "wall fresh" {
		t.Fatalf("a wall just loaded is %q", loaded)
	}

	failing.Store(true)

	broken := must(ctx, t, read, chromedp.Evaluate[chromedp.Void](`new Promise(done => setTimeout(done, 8000))`, chromedp.EvalAwaitPromise))
	if broken != "wall stale" {
		t.Fatalf("a wall answered 502 is %q; want its signs kept and greyed", broken)
	}

	failing.Store(false)

	back := must(ctx, t, read, chromedp.Evaluate[chromedp.Void](`new Promise(done => setTimeout(done, 5000))`, chromedp.EvalAwaitPromise))
	if back != "wall fresh" {
		t.Errorf("a wall whose daemon answers again is %q; want it current", back)
	}
}

// TestARevokedWallIsAskedToSignIn: a refresh answered 401 is a display revoked or a session ended, and the wall has no attention line to say so in, so it reloads, which is where a stranger is sent to sign in.
func TestARevokedWallIsAskedToSignIn(t *testing.T) {
	ctx := browser(t)

	server, _ := testPipelines(t, "app", "infra")

	var revoked atomic.Bool

	handler := server.Handler()
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if revoked.Load() && r.Header.Get("HX-Request") != "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)

			return
		}

		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(listener.Close)

	must(ctx, t, chromedp.Evaluate[chromedp.Void](`window.beforeRevoke = true`), chromedp.Navigate(listener.URL+"/wall"))

	revoked.Store(true)
	// Waited out here rather than in the page, which a reload would cut short: a 2.5s poll, with margin.
	time.Sleep(4 * time.Second)
	revoked.Store(false)

	stayed := must(ctx, t, chromedp.Evaluate[bool](`window.beforeRevoke === true`))

	if stayed {
		t.Error("a wall whose refresh is answered 401 kept showing what it last had instead of reloading to sign in")
	}
}
