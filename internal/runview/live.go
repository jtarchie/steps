package runview

import (
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
)

var escapes = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

// tailLines is how much of a running step's output the region shows: enough to see it move, few enough that eight parallel steps fit a screen.
const tailLines = 6

// Live draws a run the way buildx does. A finished step leaves one line in the terminal's own scrollback; a region at the bottom is redrawn in place with what is running now — each step's elapsed time, the last lines it printed, an agent's calls. Display only: it reads no keys, so the terminal stays the person's.
type Live struct {
	// Width is the terminal's width in columns, asked on every draw so a resize needs no signal handler.
	Width func() int
	// Height, when set, is the terminal's height in rows: a region taller than the screen cannot be moved back over, and every redraw would leave a copy of it in scrollback.
	Height func() int
	// Color allows SGR styling; cursor movement is used either way, since the region cannot be redrawn without it.
	Color bool
	// Spend, when set, is what a run has cost so far, for the header; asked when an agent step finishes, which is when spend is recorded.
	Spend func(runID string) string

	mu    sync.Mutex
	term  io.Writer
	runs  map[string]*liveRun
	order []string
	tails map[int64]*tail
	// open is the steps running now, whose bytes go to a tail; anything else written — a job-level hook, a get, an image pull before the first step — goes to scrollback, where no tail would ever show it.
	open  map[int64]bool
	loose strings.Builder
	drawn int
	held  bool
	now   func() time.Time
	stop  chan struct{}
	done  sync.WaitGroup
}

type liveRun struct {
	folder  *Folder
	job     string
	started time.Time
	spend   string
	agents  map[int64]*agentActivity
}

type agentActivity struct {
	calls int
	last  string
}

// NewLive draws onto term, which must be the terminal: the region is redrawn with cursor movement.
func NewLive(term io.Writer) *Live {
	return &Live{
		Width: func() int { return 80 },
		term:  term,
		runs:  map[string]*liveRun{},
		tails: map[int64]*tail{},
		open:  map[int64]bool{},
		now:   time.Now,
	}
}

// Start redraws on a tick because an elapsed clock must keep moving while no event arrives.
func (l *Live) Start() {
	l.stop = make(chan struct{})
	l.done.Add(1)

	go func() {
		defer l.done.Done()

		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-l.stop:
				return
			case <-ticker.C:
				l.mu.Lock()
				l.redraw()
				l.mu.Unlock()
			}
		}
	}()
}

// Stop leaves only scrollback behind: a region left on screen would read as a run still going.
func (l *Live) Stop() {
	if l.stop != nil {
		close(l.stop)
		l.done.Wait()
		l.stop = nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	l.clear()

	if l.loose.Len() > 0 {
		_, _ = io.WriteString(l.term, l.loose.String()+"\n")
		l.loose.Reset()
	}
}

// Event is the bus observer.
func (l *Live) Event(event events.Event) {
	l.mu.Lock()
	defer l.mu.Unlock()

	// A note is a line, not part of the tree, and one can arrive after its run finished (the resume hint, the usage report): folding it would bring the run back as a region nobody takes down.
	if event.Type == events.TypeStepNote {
		l.scroll(l.noteLine(event))

		return
	}

	l.scroll(l.apply(event)...)
}

// apply folds one event into its run, returning the lines it leaves in scrollback.
func (l *Live) apply(event events.Event) []string {
	run := l.run(event)
	run.folder.Add([]store.RunEventRow{rowOf(event)}, nil)

	var lines []string

	switch event.Type {
	case events.TypeStepStarted:
		if event.StepID != 0 {
			l.open[event.StepID] = true
		}
	case events.TypeStepFinished, events.TypeStepSkipped:
		delete(l.tails, event.StepID)
		delete(l.open, event.StepID)

		if step := run.step(event.StepID); step != nil {
			lines = append(lines, l.finishedLine(step))
		}

		if event.StepKind == "agent" && l.Spend != nil {
			run.spend = l.Spend(event.RunID)
		}
	case events.TypeAgentCall:
		activity := run.agents[event.StepID]
		if activity == nil {
			activity = &agentActivity{}
			run.agents[event.StepID] = activity
		}

		activity.calls++
		activity.last = event.Name
	case events.TypeJobFinished:
		lines = append(lines, l.summary(run, event)...)
		l.forget(event.RunID)
	}

	return lines
}

// Stream is where one step's bytes go: into its tail, shown while it runs. Installed as events.Output.Step.
func (l *Live) Stream(stepID int64, _ bool) io.Writer {
	return tailWriter{live: l, id: stepID}
}

// Hold takes the region off the screen and hands the terminal to a prompt until release. Installed as events.Output.Hold.
func (l *Live) Hold() (io.Writer, func()) {
	l.mu.Lock()
	l.clear()
	l.held = true
	l.mu.Unlock()

	return l.term, func() {
		l.mu.Lock()
		defer l.mu.Unlock()

		// The prompt's line was answered on the terminal, which moved the cursor past it; the region starts below.
		l.held = false
		l.redraw()
	}
}

// Log is a writer for whole log records, printed into scrollback above the region rather than through it.
func (l *Live) Log() io.Writer { return logWriter{live: l} }

func (l *Live) run(event events.Event) *liveRun {
	run := l.runs[event.RunID]
	if run == nil {
		run = &liveRun{folder: NewFolder(), job: event.Job, started: event.At, agents: map[int64]*agentActivity{}}
		if run.started.IsZero() {
			run.started = l.now()
		}

		l.runs[event.RunID] = run
		l.order = append(l.order, event.RunID)
	}

	return run
}

func (l *Live) forget(runID string) {
	delete(l.runs, runID)

	for i, id := range l.order {
		if id == runID {
			l.order = append(l.order[:i], l.order[i+1:]...)

			break
		}
	}
}

func (r *liveRun) step(id int64) *Step {
	for _, step := range r.folder.Steps() {
		if step.ID == id {
			return step
		}
	}

	return nil
}

// scroll prints lines into scrollback: the region comes off, the lines go where it was, and it is drawn again under them.
func (l *Live) scroll(lines ...string) {
	l.clear()

	for _, text := range lines {
		_, _ = io.WriteString(l.term, text+"\n")
	}

	l.redraw()
}

func (l *Live) clear() {
	if l.drawn > 0 {
		// Up to the region's first line, then erase from there to the end of the screen.
		_, _ = fmt.Fprintf(l.term, "\x1b[%dA\r\x1b[J", l.drawn)
		l.drawn = 0
	}
}

func (l *Live) redraw() {
	l.clear()

	if l.held {
		return
	}

	width := max(l.Width(), 20)

	var region []line

	for _, id := range l.order {
		run, ok := l.runs[id]
		if !ok {
			continue
		}

		view := run.folder.View(store.RunRow{})
		region = append(region, l.header(run, view))

		for _, root := range view.Roots {
			region = l.rows(region, run, root, 1)
		}
	}

	if l.Height != nil {
		// One row spare: the cursor sits on the line below the region.
		if rows := max(l.Height()-1, 1); len(region) > rows {
			region = region[:rows]
		}
	}

	for _, row := range region {
		_, _ = io.WriteString(l.term, l.style(row.sgr, clip(row.text, width-1))+"\n")
	}

	l.drawn = len(region)
}

// line is one row of the region, styled only after it is clipped so a cut never lands inside an escape.
type line struct{ sgr, text string }

func (l *Live) header(run *liveRun, view Transcript) line {
	done := 0

	for _, step := range view.Steps {
		if !step.Running() {
			done++
		}
	}

	text := fmt.Sprintf("[+] %s %s  %d/%d steps", run.job, clock(l.now().Sub(run.started)), done, len(view.Steps))
	if run.spend != "" {
		text += "  " + run.spend
	}

	return line{sgr: "1", text: text}
}

func (l *Live) rows(region []line, run *liveRun, step *Step, depth int) []line {
	if !step.Running() {
		return region
	}

	indent := strings.Repeat("  ", depth)
	text := indent + "▸ " + label(step) + "  " + clock(l.now().Sub(step.Started))

	if step.Kind == "approval" {
		text = indent + "⏸ waiting for approval · " + step.Name
	}

	if activity := run.agents[step.ID]; activity != nil {
		text += fmt.Sprintf("  %d calls · %s", activity.calls, activity.last)
	}

	region = append(region, line{text: text})

	if tail := l.tails[step.ID]; tail != nil && !step.Container() {
		for _, printed := range tail.last(step.Name) {
			region = append(region, line{sgr: "2", text: indent + "  │ " + printed})
		}
	}

	for _, child := range step.Children {
		region = l.rows(region, run, child, depth+1)
	}

	return region
}

func (l *Live) finishedLine(step *Step) string {
	name := label(step)

	switch {
	case step.Skipped():
		if step.Reason != "" {
			return l.style("2", "↷ "+name+" skipped · "+step.Reason)
		}

		return l.style("2", "↷ "+name+" skipped")
	case step.Failed():
		return l.style("31", "✗ "+name+" "+step.Status) + " · " + clock(step.Duration)
	default:
		return l.style("32", "✓") + " " + name + " · " + clock(step.Duration)
	}
}

// noteLine is plain's wording, so a note reads the same whichever renderer drew it.
func (l *Live) noteLine(event events.Event) string {
	if event.Status == events.NoteWarn {
		return l.style("33", plainLine(event))
	}

	return plainLine(event)
}

// summary is what the run leaves behind: how it came out, then — because the tail that showed it has gone — every failed step's output in full.
func (l *Live) summary(run *liveRun, event events.Event) []string {
	view := run.folder.View(store.RunRow{})
	took := time.Duration(event.DurationMS) * time.Millisecond

	lines := []string{""}

	if event.Status == "succeeded" {
		lines = append(lines, l.style("32", "✓ "+run.job+" succeeded")+fmt.Sprintf(" in %s · %d steps", clock(took), len(view.Steps)))
	} else {
		lines = append(lines, l.style("31", "✗ "+run.job+" "+event.Status)+fmt.Sprintf(" in %s · %d steps", clock(took), len(view.Steps)))
	}

	if run.spend != "" {
		lines[len(lines)-1] += " · " + run.spend
	}

	for _, step := range view.Steps {
		if !step.Failed() || step.Container() || len(step.Outputs) == 0 {
			continue
		}

		lines = append(lines, "", l.style("1", "── "+label(step)+" ──"))

		for _, output := range step.Outputs {
			lines = append(lines, strings.Split(output, "\n")...)
		}
	}

	return lines
}

func (l *Live) style(sgr, text string) string {
	if !l.Color || sgr == "" {
		return text
	}

	return "\x1b[" + sgr + "m" + text + "\x1b[0m"
}

func label(step *Step) string {
	return strings.TrimSpace(step.Kind + " " + step.Name)
}

func clock(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}

	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}

// clip cuts a line to width columns, counting runes; the text is plain, since styling comes after.
func clip(text string, width int) string {
	visible := 0

	for i := range text {
		if visible == width {
			return text[:i]
		}

		visible++
	}

	return text
}

func rowOf(event events.Event) store.RunEventRow {
	return store.RunEventRow{
		Seq: event.Seq, RunID: event.RunID, Type: event.Type,
		StepIndex: event.StepIndex, StepName: event.StepName, StepKind: event.StepKind,
		StepID: event.StepID, ParentStepID: event.ParentStepID,
		Status: event.Status, Hash: event.Hash, Text: event.Text, Name: event.Name, Detail: event.Detail,
		DurationMS: event.DurationMS, Worker: event.Worker, At: event.At,
	}
}

// tail is the last lines one step printed, with a carriage return treated the way a terminal treats it: the line starts over.
type tail struct {
	lines   []string
	partial strings.Builder
	// cr is a carriage return not yet acted on: before a newline it is a CRLF line ending, before anything else the line starting over.
	cr bool
}

func (t *tail) write(p []byte) {
	for _, b := range p {
		if t.cr && b != '\n' {
			t.partial.Reset()
		}

		t.cr = false

		switch b {
		case '\n':
			t.lines = append(t.lines, t.partial.String())
			t.partial.Reset()

			if len(t.lines) > tailLines {
				t.lines = t.lines[len(t.lines)-tailLines:]
			}
		case '\r':
			t.cr = true
		default:
			t.partial.WriteByte(b)
		}
	}
}

// last is what the region shows of the tail, without the "[label] " the runner put on each line when label is the step's own name, which the row above already says.
func (t *tail) last(label string) []string {
	shown := append([]string(nil), t.lines...)
	if t.partial.Len() > 0 {
		shown = append(shown, t.partial.String())
	}

	if len(shown) > tailLines {
		shown = shown[len(shown)-tailLines:]
	}

	for i, text := range shown {
		// A command's own colours would be cut through by clip, and bleed into every row below.
		text = escapes.ReplaceAllString(text, "")

		shown[i] = strings.TrimPrefix(text, "["+label+"] ")
	}

	return shown
}

type tailWriter struct {
	live *Live
	id   int64
}

func (w tailWriter) Write(p []byte) (int, error) {
	w.live.mu.Lock()
	defer w.live.mu.Unlock()

	if !w.live.open[w.id] {
		w.live.scrollLoose(p)

		return len(p), nil
	}

	t := w.live.tails[w.id]
	if t == nil {
		t = &tail{}
		w.live.tails[w.id] = t
	}

	t.write(p)

	return len(p), nil
}

// scrollLoose puts the whole lines of p into scrollback, keeping a partial one until its newline arrives so the region is never redrawn onto half a line.
func (l *Live) scrollLoose(p []byte) {
	l.loose.Write(p)

	text := l.loose.String()

	end := strings.LastIndexByte(text, '\n')
	if end < 0 {
		return
	}

	l.loose.Reset()
	l.loose.WriteString(text[end+1:])
	l.scroll(strings.Split(text[:end], "\n")...)
}

type logWriter struct{ live *Live }

func (w logWriter) Write(p []byte) (int, error) {
	w.live.mu.Lock()
	defer w.live.mu.Unlock()

	w.live.clear()
	_, _ = w.live.term.Write(p)
	w.live.redraw()

	return len(p), nil
}
