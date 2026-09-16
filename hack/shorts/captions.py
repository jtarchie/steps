"""Word-highlight SRT captions from the script's own words and stable-ts's word timings: captions.py <script.txt> <align.json> <out.srt> <audio-seconds>

The aligner's text is not trusted: it can emit a word twice or fuse two, and the script is what was said. Its timings are matched to the
script word by word; a word it missed gets a time interpolated between its neighbours.
"""
import json
import re
import sys

script, aligned, out, total = sys.argv[1:5]
total = float(total)
norm = lambda w: re.sub(r"[^\w']", "", w.lower())

words = open(script).read().split()
timed = [w for s in json.load(open(aligned))["segments"] for w in s["words"] if norm(w["word"])]

times = [None] * len(words)
j = 0
for i, w in enumerate(words):
    k = j
    while k < len(timed) and norm(timed[k]["word"]) != norm(w):
        k += 1
    if k < len(timed):
        times[i] = [timed[k]["start"], timed[k]["end"]]
        j = k + 1

i = 0
while i < len(words):
    if times[i] is not None:
        i += 1
        continue
    run = i
    while run < len(words) and times[run] is None:
        run += 1
    lo = times[i - 1][1] if i else 0.0
    hi = times[run][0] if run < len(words) else total
    step = (hi - lo) / (run - i)
    for n in range(i, run):
        times[n] = [lo + step * (n - i), lo + step * (n - i + 1)]
    i = run

lines, line = [], []
for i, w in enumerate(words):
    line.append(i)
    if w[-1] in ".?!:" or len(line) == 6:
        lines.append(line)
        line = []
if line:
    lines.append(line)

stamp = lambda t: "%02d:%02d:%02d,%03d" % (t // 3600, t % 3600 // 60, t % 60, round(t % 1 * 1000))
cues = []
for line in lines:
    for n, i in enumerate(line):
        start = times[i][0]
        end = times[line[n + 1]][0] if n + 1 < len(line) else times[i][1]
        if end <= start:
            end = start + 0.05
        text = " ".join('<font color="#00ff00">%s</font>' % words[k] if k == i else words[k] for k in line)
        cues.append("%d\n%s --> %s\n%s\n" % (len(cues) + 1, stamp(start), stamp(end), text))
open(out, "w").write("\n".join(cues))
