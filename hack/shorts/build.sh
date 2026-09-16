#!/usr/bin/env bash
# Renders one short to <short>/out/<short>.mp4: hack/shorts/build.sh <short-dir>
#
# A short is a directory of scenes, scenes/NN-name.txt is the narration script for scene NN, and the scene's visual is the
# sibling with the same stem: .html (a slide, screenshotted), .tape (VHS, run in the work dir beside the pipeline), or .mjs
# (a browser script given STEPS_URL and OUT). Your recording goes in narration/NN-name.<ext>; without one, `say` reads the
# script so the pipeline still renders. A scene lasts as long as its narration (a clip shorter than that holds its last
# frame); captions come from aligning the script text to the audio, so they say exactly what the script says.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
short=$(cd "${1:?usage: build.sh <short-dir>}" && pwd)
name=$(basename "$short")
steps=${STEPS:-$here/../../steps}
llm_port=${LLM_PORT:-8377}
web_port=${WEB_PORT:-8378}
voice=${VOICE:-Samantha}
# With ELEVENLABS_API_KEY set, narration is synthesized in this voice; the key is never written anywhere.
tts_voice=${ELEVENLABS_VOICE_ID:-aMSt68OGf4xUZAnLpTU8}
tts_model=${ELEVENLABS_MODEL:-eleven_multilingual_v2}
cache=$here/.cache
# VHS 0.12.0 (Homebrew's current bottle) exits 0 and writes nothing (charmbracelet/vhs#787); 0.11.0 built from source records fine.
vhs=${VHS:-vhs}
fps=30

test -x "$steps" || { echo "no steps binary at $steps (go build, or set STEPS=)" >&2; exit 1; }
for tool in "$vhs" stable-ts node say; do
  command -v "$tool" >/dev/null || { echo "missing $tool" >&2; exit 1; }
done

# Homebrew's ffmpeg 9 is a slim build with no libass, and captions need it: brew install ffmpeg-full, or uv tool install static-ffmpeg.
# The real static binary rather than the Python shim, so ffprobe sits beside it. grep -q would close the pipe early and, under pipefail, fail the probe on a capable binary.
ffmpeg=""
for candidate in ${FFMPEG:-} ffmpeg $(static_ffmpeg_paths 2>/dev/null | sed -n 's/^FFMPEG=//p'); do
  if "$candidate" -hide_banner -filters 2>/dev/null | grep " subtitles " >/dev/null; then ffmpeg=$candidate; break; fi
done
test -n "$ffmpeg" || { echo "no ffmpeg with the subtitles filter (brew install ffmpeg-full, or uv tool install static-ffmpeg)" >&2; exit 1; }
ffprobe=$(dirname "$(command -v "$ffmpeg")")/ffprobe

out=$short/out tmp=$short/out/tmp work=$short/out/work
rm -rf "$out"
mkdir -p "$tmp" "$work"

export STEPS_DEMO_KEY=demo
PATH="$(cd "$(dirname "$steps")" && pwd):$PATH"
export PATH

# One daemon and one scripted model serve every scene, so the terminal take and the browser take show the same run.
git clone -q --bare "$(cd "$here/../.." && pwd)" "$work/steps.git"
sed -e "s/__PORT__/$llm_port/" -e "s|__REPO__|$work/steps.git|" "$short/review.yml" >"$work/review.yml"
node "$here/fakellm.mjs" "$llm_port" "$short/turns.json" &
llm=$!
"$steps" web --db "$work/web.db" --listen "127.0.0.1:$web_port" --no-preflight >"$tmp/web.log" 2>&1 &
web=$!
trap 'kill "$llm" "$web" 2>/dev/null' EXIT

for _ in $(seq 1 50); do
  curl -sf "http://127.0.0.1:$web_port/" >/dev/null 2>&1 && break
  sleep 0.2
done
"$steps" pipeline set -p review -c "$work/review.yml" -n --target "http://127.0.0.1:$web_port" --log-level warn >/dev/null

# Commas inside a filter argument are escaped, or the graph parser reads them as the next filter. Sizes and margins are in the SRT's 288-line PlayRes, so scale by 1920/288.
captions='FontName=Helvetica Neue\\,FontSize=13\\,Bold=1\\,PrimaryColour=&H00FFFFFF\\,OutlineColour=&H00000000\\,Outline=2\\,Shadow=0\\,MarginV=45\\,Alignment=2'
# Top-aligned, so a browser shot shorter than the frame leaves its empty band at the bottom under the captions.
fit="scale=1080:1920:force_original_aspect_ratio=decrease,pad=1080:1920:(ow-iw)/2:0"

: >"$tmp/scenes.ffconcat"
echo "ffconcat version 1.0" >>"$tmp/scenes.ffconcat"

for script in "$short"/scenes/[0-9][0-9]-*.txt; do
  n=$(basename "${script%.txt}")
  echo "== $n"

  audio=$(find "$short/narration" -name "$n.*" 2>/dev/null | head -1)
  if [ -z "$audio" ] && [ -n "${ELEVENLABS_API_KEY:-}" ]; then
    # Keyed by voice, model and text, so a rebuild spends credits only on a line that changed.
    audio=$cache/$n-$({ echo "$tts_voice $tts_model"; cat "$script"; } | shasum -a 256 | cut -c1-16).mp3
    if [ ! -s "$audio" ]; then
      mkdir -p "$cache"
      body=$(python3 -c 'import json,sys; print(json.dumps({"text": open(sys.argv[1]).read().strip(), "model_id": sys.argv[2]}))' "$script" "$tts_model")
      curl -sf -o "$audio" -X POST "https://api.elevenlabs.io/v1/text-to-speech/$tts_voice?output_format=mp3_44100_128" \
        -H "xi-api-key: $ELEVENLABS_API_KEY" -H "Content-Type: application/json" -d "$body" \
        || { rm -f "$audio"; echo "$n: ElevenLabs refused the request" >&2; exit 1; }
    fi
  fi
  if [ -z "$audio" ]; then
    say -v "$voice" -o "$tmp/$n.aiff" -f "$script"
    audio=$tmp/$n.aiff
  fi
  "$ffmpeg" -y -loglevel error -i "$audio" -ar 48000 -ac 2 "$tmp/$n.wav"
  adur=$("$ffprobe" -v error -show_entries format=duration -of csv=p=0 "$tmp/$n.wav")

  # Forced alignment of the script, not transcription: the captions are your words, timed to your voice. Only the timings are used (captions.py).
  stable-ts "$tmp/$n.wav" --align "$script" --language en --model base.en -o "$tmp/$n.json" -y >"$tmp/$n.align.log" 2>&1
  python3 "$here/captions.py" "$script" "$tmp/$n.json" "$tmp/$n.srt" "$adur"

  # The visual: a still (or stills) held for the narration, or a clip padded out to it.
  stem=$short/scenes/$n
  pad=0
  if [ -f "$stem.html" ]; then
    node "$here/render.mjs" "$stem.html" "$tmp/$n-1.png" >/dev/null
  elif [ -f "$stem.tape" ]; then
    # VHS's parser rejects an absolute output path, and the tape runs in the work dir; tmp is its sibling.
    (cd "$work" && "$vhs" "$stem.tape" -o "../tmp/$n.raw.mp4" >"$tmp/$n.vhs.log" 2>&1)
    test -s "$tmp/$n.raw.mp4" || { echo "$n: vhs wrote no video" >&2; cat "$tmp/$n.vhs.log" >&2; exit 1; }
  elif [ -f "$stem.mjs" ]; then
    STEPS_URL="http://127.0.0.1:$web_port" OUT="$tmp/$n" node "$stem.mjs" >/dev/null
  else
    echo "$n: no visual (want $n.html, $n.tape or $n.mjs)" >&2
    exit 1
  fi

  if [ -f "$tmp/$n.raw.mp4" ]; then
    vdur=$("$ffprobe" -v error -show_entries format=duration -of csv=p=0 "$tmp/$n.raw.mp4")
    dur=$(echo "if ($adur > $vdur) $adur else $vdur" | bc -l)
    pad=$(echo "$dur - $vdur" | bc -l)
    src=(-i "$tmp/$n.raw.mp4")
  else
    dur=$adur
    # The concat demuxer does not honor the last entry's duration, so the still is cloned out to the narration instead.
    pad=$dur
    stills=("$tmp/$n"-*.png)
    each=$(echo "$dur / ${#stills[@]}" | bc -l)
    { echo "ffconcat version 1.0"; for png in "${stills[@]}"; do echo "file '$png'"; echo "duration $each"; done; } >"$tmp/$n.stills.ffconcat"
    src=(-f concat -safe 0 -i "$tmp/$n.stills.ffconcat")
  fi

  "$ffmpeg" -y -loglevel error "${src[@]}" -i "$tmp/$n.wav" \
    -filter_complex "[0:v]$fit,tpad=stop_mode=clone:stop_duration=$pad,fps=$fps,subtitles=$tmp/$n.srt:force_style='$captions',format=yuv420p[v];[1:a]apad[a]" \
    -map "[v]" -map "[a]" -t "$dur" -c:v libx264 -preset medium -crf 18 -c:a aac -b:a 160k -movflags +faststart "$tmp/$n.mp4"

  echo "file '$tmp/$n.mp4'" >>"$tmp/scenes.ffconcat"
done

"$ffmpeg" -y -loglevel error -f concat -safe 0 -i "$tmp/scenes.ffconcat" -c copy "$out/$name.mp4"
echo "$out/$name.mp4"
