(function () {
  // The switcher is inside #statusline, which every polling page now morphs
  // out of band so the attention marks on the OTHER pipelines stay live. That
  // makes an open menu reader state the server does not know about — like a
  // fold on the run page — so it is recorded here and re-applied after a
  // swap, or a reader who opened it watches it shut itself 2.5s later.
  //
  // Delegated rather than bound, for the same reason the step toggles are:
  // the morph replaces these elements, and a listener bound to the copy that
  // was on the page at load is a listener on a node nothing can click.
  var switcherOpen = false;

  function paintSwitcher() {
    var pipebtn = document.getElementById('pipebtn');
    var pipemenu = document.getElementById('pipemenu');
    if (!pipebtn || !pipemenu) return;
    pipemenu.classList.toggle('open', switcherOpen);
    pipebtn.setAttribute('aria-expanded', String(switcherOpen));
  }

  document.addEventListener('click', function (e) {
    switcherOpen = e.target.closest('#pipebtn') ? !switcherOpen : false;
    paintSwitcher();
  });

  // What the reader has folded or unfolded, and which row they are on, are
  // the two pieces of state the SERVER cannot know — and a swap reconciles
  // the row to what the server drew, so both are lost the moment a step
  // changes. Recorded as the reader makes them and re-applied after every
  // swap, which is what keeps a live transcript readable while it grows.
  //
  // Kept in sessionStorage per run as well, because the page reloads itself
  // when a run ends — the moment the server folds every row the reader did
  // not touch — and a Map alone forgets exactly the rows they did.
  var folds = new Map();
  var foldsKey = 'steps.folds:' + window.location.pathname;

  try {
    var stored = JSON.parse(window.sessionStorage.getItem(foldsKey) || '[]');
    if (Array.isArray(stored)) {
      stored.forEach(function (pair) {
        if (Array.isArray(pair) && typeof pair[0] === 'string' && typeof pair[1] === 'boolean') folds.set(pair[0], pair[1]);
      });
    }
  } catch (err) {}

  // Storage throws in private mode, when disabled and when full; the Map
  // still carries the folds for this page's life. Full is the one worth a
  // retry: every run this tab visited keeps a key, and those are what gets
  // dropped so the run on screen still survives its closing reload.
  function persist() {
    var value = JSON.stringify(Array.from(folds));
    try { window.sessionStorage.setItem(foldsKey, value); return; } catch (err) {}
    try {
      for (var i = window.sessionStorage.length - 1; i >= 0; i--) {
        var key = window.sessionStorage.key(i);
        if (key && key !== foldsKey && key.indexOf('steps.folds:') === 0) window.sessionStorage.removeItem(key);
      }
      window.sessionStorage.setItem(foldsKey, value);
    } catch (err) {}
  }

  function remember(key, open) {
    folds.set(key, open);
    persist();
  }

  function applyFolds() {
    folds.forEach(function (open, key) {
      var step = document.querySelector('[data-step="' + CSS.escape(key) + '"]');
      if (step) step.classList.toggle('open', open);
    });
  }

  // A shared link names a step; open it so the reader lands on the content
  // rather than on a collapsed row they still have to click. Here rather than
  // in the live-run script, which is where it used to live: the link a person
  // actually pastes is read days later, against a run that finished. Adding
  // the class (rather than opening the body from :target in CSS) is what keeps
  // the chevron honest and the row still collapsible.
  function openHashTarget() {
    if (!window.location.hash) return;
    // Guarded because a fragment is not necessarily a selector: '#2' is a
    // SyntaxError, not a miss, and the throw would escape this IIFE and take
    // every listener registered below it — the fold restore, the toggle
    // handlers, j/k navigation, the palette — with it, on every page.
    var target = null;
    try { target = document.querySelector(window.location.hash); } catch (err) { return; }
    if (!target || !target.classList.contains('step')) return;
    // Its containers too: a passed block is drawn closed, and a row inside
    // one is hidden however open it is. Recorded like any other fold, or the
    // next swap of a live run closes it.
    reveal(target);
  }

  // Before the hash, so a shared link wins over a remembered fold.
  applyFolds();
  openHashTarget();
  window.addEventListener('hashchange', openHashTarget);

  // DELEGATED, deliberately. Bound per-element at load, a step the live
  // stream appended later had no listener at all: it rendered with a chevron,
  // looked expandable, and did nothing when clicked — so an agent whose row
  // collapsed as it finished could only be reopened by reloading the page.
  // One listener on the document covers every step, including the ones that
  // do not exist yet.
  function toggleFrom(target) {
    var head = target.closest('.stephead');
    if (!head) return null;
    var step = head.parentElement;
    return step && step.hasAttribute('data-toggle') ? step : null;
  }

  function fold(step) {
    step.classList.toggle('open');
    if (step.dataset.step) remember(step.dataset.step, step.classList.contains('open'));
  }

  // <head> is never swapped, so the tab's title and icon follow the #mark
  // the server drew into the statusline, copied rather than recomputed.
  function applyMark() {
    var mark = document.getElementById('mark');
    if (!mark) return;
    var icon = document.getElementById('favicon');
    if (icon && mark.dataset.icon && icon.getAttribute('href') !== mark.dataset.icon) icon.setAttribute('href', mark.dataset.icon);
    if (mark.dataset.title && document.title !== mark.dataset.title) document.title = mark.dataset.title;
  }

  document.addEventListener('htmx:after:swap', function () {
    applyMark();
    paintSwitcher();
    applyFolds();

    if (chosen) {
      var step = document.querySelector('[data-step="' + CSS.escape(chosen) + '"]');
      if (step) step.classList.add('sel');
    }
  });

  document.addEventListener('click', function (e) {
    if (e.target.closest('a')) return;
    var step = toggleFrom(e.target);
    if (step) fold(step);
  });

  // The server folds a block shut under the reader once it passes; a row
  // they are on keeps its containers open, or it vanishes and focus falls to
  // the body.
  document.addEventListener('focusin', function (e) {
    var head = e.target.closest && e.target.closest('.stephead');
    if (head && head.parentElement && head.parentElement.parentElement) reveal(head.parentElement.parentElement);
  });

  document.addEventListener('keydown', function (e) {
    if (e.key !== 'Enter' && e.key !== ' ') return;
    var step = toggleFrom(e.target);
    if (!step) return;
    e.preventDefault();
    fold(step);
  });

  // Keyboard navigation over the step tree.
  //
  // A run of any size is read by scanning, and on a tree the scan is the
  // expensive part: the interesting row is three containers deep and the page
  // is tall. j/k walk it, e/c change what is open wholesale, and f goes
  // straight to the thing a person opened a failed run to find.
  var transcript = document.querySelector('.transcript');
  var chosen = null;

  // Not #run-empty: the placeholder is a `.step` that is always in the DOM
  // and hidden by CSS once a real row exists, so an unfiltered walk lands the
  // reader on an invisible row — mid-list, because the stream appends after
  // it — and drops the selection there.
  function steps() { return transcript ? Array.from(transcript.querySelectorAll('.step:not(#run-empty)')) : []; }

  function select(step) {
    if (!step) return;
    steps().forEach(function (other) { other.classList.remove('sel'); });
    step.classList.add('sel');
    chosen = step.dataset.step || null;
    // Focused as well as marked, so Enter and Space reach the toggle handler
    // above rather than needing a second copy of it here.
    var head = step.querySelector(':scope > .stephead');
    if (head && head.hasAttribute('tabindex')) head.focus({preventScroll: true});
    step.scrollIntoView({block: 'center', behavior: 'smooth'});
  }

  function selected() { return transcript ? transcript.querySelector('.step.sel') : null; }

  // Visible rows only: a row inside a closed block is display:none, and
  // selecting it loses the reader. f and e still see every row.
  function step(by) {
    var all = steps().filter(function (node) { return !node.parentElement.closest('.step.container:not(.open)'); });
    if (!all.length) return;
    // A selection folded out of sight moves from the block that hid it,
    // rather than jumping back to the top.
    var from = selected();
    while (from && all.indexOf(from) < 0) from = from.parentElement && from.parentElement.closest('.step');
    var at = all.indexOf(from);
    select(all[Math.min(Math.max(at + by, 0), all.length - 1)]);
  }

  function foldAll(open) {
    steps().forEach(function (node) {
      if (!node.hasAttribute('data-toggle')) return;
      node.classList.toggle('open', open);
      if (node.dataset.step) folds.set(node.dataset.step, open);
    });
    // Once, not per row: each write serializes every fold, so per row was
    // quadratic on a large transcript.
    persist();
  }

  // The INNERMOST failure: a failed step inside a failed container is the one
  // that actually broke, and every ancestor is failed only because of it.
  function firstFailure() {
    return steps().filter(function (node) {
      return node.classList.contains('failed') && !node.querySelector('.step.failed');
    })[0];
  }

  function reveal(node) {
    for (var at = node; at; at = at.parentElement) {
      if (!at.classList || !at.classList.contains('step')) continue;
      at.classList.add('open');
      if (at.dataset.step) folds.set(at.dataset.step, true);
    }
    persist();
  }

  document.addEventListener('keydown', function (e) {
    if (!transcript || e.metaKey || e.ctrlKey || e.altKey) return;
    if (e.target.tagName === 'INPUT' || e.target.tagName === 'TEXTAREA') return;

    switch (e.key) {
      case 'j': step(1); break;
      case 'k': step(-1); break;
      case 'e': foldAll(true); break;
      case 'c': foldAll(false); break;
      case 'f':
        var failure = firstFailure();
        if (failure) { reveal(failure); select(failure); }
        break;
      default: return;
    }

    e.preventDefault();
  });

  var backdrop = document.getElementById('palette-backdrop');
  var palette = document.getElementById('palette');
  var input = document.getElementById('palette-input');
  var list = document.getElementById('palette-list');
  var hits = [];
  var sel = 0;

  // Built as nodes, not as markup. Every value here is a job name, a run id
  // or a pipeline slug, and none of them is validated -- a job name may hold
  // any characters at all. Since the palette began answering about OTHER
  // served pipelines, a name written in one pipeline renders on every other
  // pipeline's page, so an innerHTML template would let a repo you merely
  // serve alongside your own run script on a page you never left.
  function draw() {
    list.textContent = '';
    hits.forEach(function (h, i) {
      var li = document.createElement('li');
      if (i === sel) { li.className = 'sel'; }
      li.dataset.url = h.url;
      li.appendChild(el('span', 'kind', h.kind));
      li.appendChild(el('span', '', h.name));
      li.appendChild(el('span', 'hint', h.hint || ''));
      li.addEventListener('click', function () { window.location = li.dataset.url; });
      list.appendChild(li);
    });
  }

  function el(tag, cls, text) {
    var node = document.createElement(tag);
    if (cls) { node.className = cls; }
    node.textContent = text;
    return node;
  }

  var pending;
  // Debouncing bounds how OFTEN a search is sent; it does not order the
  // answers. One search now fans out over every served pipeline's store, and
  // under a shared --db file those queries serialize on one connection, so
  // an earlier keystroke can genuinely answer after a later one and redraw
  // the palette with results for a query the operator has moved past. The
  // token is compared before anything is assigned.
  var generation = 0;
  function search() {
    clearTimeout(pending);
    pending = setTimeout(function () {
      var mine = ++generation;
      fetch(palette.dataset.search + '?q=' + encodeURIComponent(input.value))
        .then(function (r) { return r.json(); })
        .then(function (data) {
          if (mine !== generation) { return; }
          hits = data || []; sel = 0; draw();
        })
        // Without this a non-JSON body (an error page) throws inside the
        // promise and the palette sits on whatever it drew last, saying
        // nothing.
        .catch(function () {
          if (mine !== generation) { return; }
          hits = []; sel = 0; draw();
        });
    }, 80);
  }

  function open() {
    backdrop.classList.add('open'); palette.classList.add('open');
    input.value = ''; input.focus(); search();
  }
  function close() {
    backdrop.classList.remove('open'); palette.classList.remove('open');
  }

  backdrop.addEventListener('click', close);
  input.addEventListener('input', search);
  input.addEventListener('keydown', function (e) {
    if (e.key === 'ArrowDown') { sel = Math.min(sel + 1, hits.length - 1); draw(); e.preventDefault(); }
    else if (e.key === 'ArrowUp') { sel = Math.max(sel - 1, 0); draw(); e.preventDefault(); }
    else if (e.key === 'Enter' && hits[sel]) { window.location = hits[sel].url; }
    else if (e.key === 'Escape') { close(); }
  });
  document.addEventListener('keydown', function (e) {
    if (e.key === '/' && !palette.classList.contains('open') && !e.target.closest('input, textarea')) {
      e.preventDefault(); open();
    }
  });
  // Delegated, like the switcher: the button lives in #statusline, which
  // every polling page morphs, and a listener bound at load is bound to a
  // node the morph may have replaced.
  document.addEventListener('click', function (e) {
    if (e.target.closest('#jumpbtn')) open();
  });
})();

// On a phone the tabs are a strip that scrolls sideways, and the current one
// can start off the right edge — the one tab a reader most needs to see is
// the one telling them where they are. block:'nearest' keeps this from also
// scrolling the page. Once, at load: after that the strip is theirs.
(function () {
  var current = document.querySelector('.tabs .tab[aria-current="page"]');
  var strip = current && current.parentElement;
  if (strip && strip.scrollWidth > strip.clientWidth) current.scrollIntoView({inline: 'center', block: 'nearest'});
})();

// Keep relative times honest. The server renders the text once; a page left
// open — which is the whole point of the live view — would otherwise keep
// claiming a run started "4s ago" an hour later. Thresholds mirror
// formatDuration in render.go so a reload never visibly reformats anything.
(function () {
  function fmt(ms) {
    if (ms <= 0) return '—';
    if (ms < 1000) return Math.round(ms) + 'ms';
    if (ms < 60000) return (ms / 1000).toFixed(1) + 's';
    var m = Math.floor(ms / 60000);
    var s = Math.floor(ms / 1000) % 60;
    if (m < 60) return m + 'm ' + String(s).padStart(2, '0') + 's';
    return Math.floor(m / 60) + 'h ' + String(m % 60).padStart(2, '0') + 'm';
  }

  function tick() {
    var now = Date.now();
    document.querySelectorAll('time[data-ago]').forEach(function (node) {
      node.textContent = fmt(now - Date.parse(node.dataset.ago)) + ' ago';
    });
    document.querySelectorAll('time[data-elapsed-since]').forEach(function (node) {
      node.textContent = fmt(now - Date.parse(node.dataset.elapsedSince));
    });
    // The countdown counterpart: how long until a running agent step's
    // resolved timeout: expires, rather than how long it has run. Floored at
    // "expired" instead of ticking on into a negative duration, which fmt
    // does not format.
    document.querySelectorAll('time[data-deadline]').forEach(function (node) {
      var remaining = Date.parse(node.dataset.deadline) - now;
      node.textContent = remaining <= 0 ? 'expired' : fmt(remaining);
    });
  }

  // A second only when something is actively counting; otherwise half a
  // minute is plenty for text measured in minutes and hours.
  var live = document.querySelector('time[data-elapsed-since], time[data-deadline]') !== null;
  setInterval(tick, live ? 1000 : 30000);
})();
