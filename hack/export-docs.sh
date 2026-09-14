#!/usr/bin/env bash
# Renders docs/*.md to static HTML for stepsci.dev through a throwaway `steps web`, so the site and /docs share one renderer: hack/export-docs.sh <out-dir>

set -euo pipefail

out=${1:?usage: hack/export-docs.sh <out-dir>}
steps=${STEPS:-./steps}
port=${PORT:-8199}
repo_url=https://github.com/jtarchie/steps/blob/main

test -x "$steps" || { echo "no steps binary at $steps (build it, or set STEPS=)" >&2; exit 1; }
mkdir -p "$out"

state=$(mktemp -d)
# || true: under set -e, killing a daemon that already died would abort the trap before the rm.
trap 'kill "$daemon" 2>/dev/null || true; rm -rf "$state"' EXIT

# Empty state and --read-only: nothing to poll, nothing to run, no controls to strip later.
"$steps" web --db "$state/docs.db" --read-only --listen "127.0.0.1:$port" >"$state/web.log" 2>&1 &
daemon=$!

for _ in $(seq 1 50); do
  curl -sf "http://127.0.0.1:$port/docs/README.md" >/dev/null 2>&1 && break
  sleep 0.2
done
# Another server already on the port would answer the curl; a daemon that lost the bind has exited, so ask it too.
{ kill -0 "$daemon" 2>/dev/null && curl -sf "http://127.0.0.1:$port/docs/README.md" >/dev/null; } || { cat "$state/web.log" >&2; exit 1; }

curl -sf "http://127.0.0.1:$port/static/app.css" >"$out/steps.css"

# The directory is the page list, so a new page ships next export without editing this file.
for md in docs/*.md; do
  page=$(basename "$md")
  html="$out/${page%.md}.html"
  # Top Banana accepts lowercase paths only, and a directory's index is what /docs/ serves anyway.
  if [ "$page" = README.md ]; then html="$out/index.html"; fi
  curl -sf "http://127.0.0.1:$port/docs/$page" >"$state/page.html"

  # Everything outside <main> is the daemon's chrome: status bar, jump palette, htmx.
  perl -0777 -ne 'print $1 if m{<main>(.*?)</main>}s' "$state/page.html" >"$state/main.html"
  test -s "$state/main.html" || { echo "$page: no <main> in the rendered page" >&2; exit 1; }

  title=$(perl -0777 -ne 'print $1 if m{<h1[^>]*>(.*?)</h1>}s' "$state/main.html" | perl -pe 's/<[^>]+>//g')
  : "${title:=${page%.md}}"

  # Absolute /docs/ links first so the relative rule below cannot double-rewrite them; ../ climbs out of docs/ (schema, examples/) and goes to GitHub. Then chroma's per-token inline styles become classes: a quarter of every page was the same eight attributes repeated.
  perl -pe '
    s{href="/docs/([A-Za-z0-9._-]+)\.md(#[^"]*)?"}{href="$1.html$2"}g;
    s{href="([A-Za-z0-9._-]+)\.md(#[^"]*)?"}{href="$1.html$2"}g;
    s{href="README\.html(#[^"]*)?"}{href="index.html$1"}g;
    s{href="\.\./([^"]+)"}{href="'"$repo_url"'/$1"}g;
    s{<pre style="color:#d8d5c9;background-color:#171a16;-webkit-text-size-adjust:none;">}{<pre class="hl">}g;
    s{<span style="display:flex;">}{<span class="l">}g;
    s{<span style="color:#83887b;font-style:italic">}{<span class="c">}g;
    s{<span style="color:#7aa4d9">}{<span class="k">}g;
    s{<span style="color:#83887b">}{<span class="p">}g;
    s{<span style="color:#84c06d">}{<span class="s">}g;
    s{<span style="color:#d9a94a">}{<span class="n">}g;
    s{<span style="color:#b98fcc">}{<span class="w">}g;
    s{<span style="color:#6fbcb4">}{<span class="f">}g;
    s{<span style="color:#e0645a">}{<span class="d">}g;
    s{<span style="font-weight:bold">}{<span class="b">}g;
    s{<span style="font-style:italic">}{<span class="i">}g;
  ' "$state/main.html" | perl -0777 -pe 's{</span></span>(?=<span class="l">|</code>)}{}g; s{<span class="l"><span>}{}g' >"$state/body.html"

  {
    cat <<EOF
<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>$title · steps docs</title>
<link rel="stylesheet" href="steps.css">
<style>.hl{color:#d8d5c9;background-color:#171a16}.c{color:#83887b;font-style:italic}.k{color:#7aa4d9}.p{color:#83887b}.s{color:#84c06d}.n{color:#d9a94a}.w{color:#b98fcc}.f{color:#6fbcb4}.d{color:#e0645a}.b{font-weight:bold}.i{font-style:italic}</style>
</head>
<body>
<div class="statusline">
  <a class="brand" href="/"><b>steps</b> docs</a>
  <nav class="tabs" aria-label="Sections">
    <a class="tab" href="/">site</a>
    <a class="tab" aria-current="page" href="index.html">docs</a>
    <a class="tab" href="https://github.com/jtarchie/steps">github</a>
    <a class="tab" href="https://github.com/jtarchie/steps/releases">releases</a>
  </nav>
</div>
<main>
EOF
    cat "$state/body.html"
    cat <<'EOF'
</main>
</body>
</html>
EOF
  } >"$html"
  echo "$html"
done
