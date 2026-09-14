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
trap 'kill "$daemon" 2>/dev/null; rm -rf "$state"' EXIT

# Empty state and --read-only: nothing to poll, nothing to run, no controls to strip later.
"$steps" web --db "$state/docs.db" --read-only --listen "127.0.0.1:$port" >"$state/web.log" 2>&1 &
daemon=$!

for _ in $(seq 1 50); do
  curl -sf "http://127.0.0.1:$port/docs/README.md" >/dev/null 2>&1 && break
  sleep 0.2
done
curl -sf "http://127.0.0.1:$port/docs/README.md" >/dev/null || { cat "$state/web.log" >&2; exit 1; }

curl -sf "http://127.0.0.1:$port/static/app.css" >"$out/steps.css"

# The directory is the page list, so a new page ships next export without editing this file.
for md in docs/*.md; do
  page=$(basename "$md")
  html="$out/${page%.md}.html"
  curl -sf "http://127.0.0.1:$port/docs/$page" >"$state/page.html"

  # Everything outside <main> is the daemon's chrome: status bar, jump palette, htmx.
  perl -0777 -ne 'print $1 if m{<main>(.*?)</main>}s' "$state/page.html" >"$state/main.html"
  test -s "$state/main.html" || { echo "$page: no <main> in the rendered page" >&2; exit 1; }

  title=$(perl -0777 -ne 'print $1 if m{<h1[^>]*>(.*?)</h1>}s' "$state/main.html" | perl -pe 's/<[^>]+>//g')
  : "${title:=${page%.md}}"

  # Absolute /docs/ links first so the relative rule below cannot double-rewrite them; ../ climbs out of docs/ (schema, examples/) and goes to GitHub.
  perl -pe '
    s{href="/docs/([A-Za-z0-9._-]+)\.md"}{href="$1.html"}g;
    s{href="([A-Za-z0-9._-]+)\.md(#[^"]*)?"}{href="$1.html$2"}g;
    s{href="\.\./([^"]+)"}{href="'"$repo_url"'/$1"}g;
  ' "$state/main.html" >"$state/body.html"

  {
    cat <<EOF
<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>$title · steps docs</title>
<link rel="stylesheet" href="steps.css">
</head>
<body>
<div class="statusline">
  <a class="brand" href="https://stepsci.dev"><b>steps</b> docs</a>
  <nav class="tabs" aria-label="Sections">
    <a class="tab" href="https://stepsci.dev">site</a>
    <a class="tab" aria-current="page" href="README.html">docs</a>
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
