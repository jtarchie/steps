package web

import (
	"net/http/httptest"
	"testing"

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
