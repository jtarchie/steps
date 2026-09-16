# hack/shorts: vertical demo videos from code

Each subdirectory is one YouTube Short. `build.sh <dir>` renders it to `<dir>/out/<dir>.mp4` (1080x1920, 30fps, H.264 + AAC, captions burned in). Nothing here is shipped code; the outputs are gitignored.

## How a short is built

- **The narration script is the spine.** `scenes/NN-name.txt` is what is said over scene NN. A scene lasts exactly as long as its narration, so re-recording one line changes one scene and nothing else has to move.
- **The visual is the sibling with the same stem.** `NN-name.html` is a slide (screenshotted by `render.mjs` at 1080x1920). `NN-name.tape` is a VHS terminal recording, run in `out/work/` beside the pipeline. `NN-name.mjs` is a browser script given `STEPS_URL` and `OUT` and writes `OUT-1.png`, `OUT-2.png`, ... which are held for equal shares of the narration.
- **Your voice goes in `narration/NN-name.<ext>`** (any format ffmpeg reads). Without one, macOS `say` reads the script, so the pipeline renders end to end before you have recorded anything.
- **Captions are forced alignment, not transcription.** `stable-ts --align` times the script's own words to the audio, so the captions say exactly what the script says and highlight word by word.
- **The model is scripted.** `fakellm.mjs` is an OpenAI-compatible endpoint that answers from `turns.json`, the same seam the e2e tests use, so a take re-renders byte-identical after a UI change. `review.yml` points its agent at it; `__PORT__` is filled in by `build.sh`. One `steps web` and one fake model serve every scene, so the terminal take and the browser take show the same run.

## Toolchain

- `vhs`: Homebrew's 0.12.0 exits 0 and writes nothing ([charmbracelet/vhs#787](https://github.com/charmbracelet/vhs/issues/787)). Build 0.11.0 from source and pass `VHS=`: `git clone -b v0.11.0 https://github.com/charmbracelet/vhs && cd vhs && go get github.com/dlclark/regexp2@v1.12.0 && go build -o ~/go/bin/vhs .` (the `go get` is because the regexp2 tag 0.11.0 pins is an empty module).
- `ffmpeg` with libass: Homebrew's `ffmpeg` 9 is a slim build with no `subtitles` filter. `brew install ffmpeg-full`, or `uv tool install static-ffmpeg` (the build probes for the filter and prefers whichever has it; `FFMPEG=` overrides).
- `stable-ts`: `uv tool install stable-ts --with mlx-whisper`.
- `node` and `npm install` in this directory (cloakbrowser, a Playwright-flavored Chromium; its first run downloads the browser).
- `steps`: `go build` at the repo root, or `STEPS=`.

```bash
VHS=~/go/bin/vhs ./build.sh verdicts
```
