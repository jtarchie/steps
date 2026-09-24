package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/lmittmann/tint"
	"golang.org/x/sys/unix"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/runview"
	"github.com/jtarchie/steps/internal/store"
)

// ProgressFlags chooses how a run is drawn on the terminal that started it.
type ProgressFlags struct {
	Progress string `default:"auto" enum:"auto,tty,plain" help:"how to draw the run: tty is a live view, plain one line per event; auto is tty on a terminal outside CI" name:"progress"`
}

// wantsLive is auto's answer too: a live region is only for a person at a terminal, and a CI log, a pipe or an agent calling steps gets lines it can read back.
func (p ProgressFlags) wantsLive() bool {
	switch p.Progress {
	case "tty":
		return true
	case "plain":
		return false
	}

	return stdoutIsTerminal() && os.Getenv("CI") == "" && os.Getenv("TERM") != "dumb"
}

// draw installs the live view when one is wanted and returns what takes it down; with none, RunJob prints plainly on its own.
func (p ProgressFlags) draw(ctx context.Context, st store.Usage) (context.Context, func()) {
	if !p.wantsLive() {
		return ctx, func() {}
	}

	live := runview.NewLive(os.Stdout)
	live.Width = func() int { return terminalSize(80, func(size *unix.Winsize) uint16 { return size.Col }) }
	live.Height = func() int { return terminalSize(24, func(size *unix.Winsize) uint16 { return size.Row }) }
	live.Color = os.Getenv("NO_COLOR") == ""
	live.Spend = func(runID string) string { return runSpend(ctx, st, runID) }

	ctx = events.WithRenderer(ctx, live.Event)
	ctx = events.WithOutput(ctx, events.Output{Step: live.Stream, Hold: live.Hold})

	// Log records go above the region too, or the first warning a step logs tears it.
	previous := slog.Default()
	slog.SetDefault(slog.New(tint.NewTextHandler(live.Log(), &tint.Options{
		Level:     levelOf(previous.Handler()),
		AddSource: true,
		NoColor:   !live.Color,
	})))

	live.Start()

	return ctx, func() {
		live.Stop()
		slog.SetDefault(previous)
	}
}

func levelOf(handler slog.Handler) slog.Level {
	for _, level := range []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelWarn} {
		if handler.Enabled(context.Background(), level) {
			return level
		}
	}

	return slog.LevelError
}

// terminalSize is one dimension of stdout's window, or fallback when the terminal will not say.
func terminalSize(fallback int, dimension func(*unix.Winsize) uint16) int {
	size, err := unix.IoctlGetWinsize(int(os.Stdout.Fd()), unix.TIOCGWINSZ)
	if err != nil || dimension(size) == 0 {
		return fallback
	}

	return int(dimension(size))
}

// runSpend is the header's cost: tokens always, dollars when something reported a price.
func runSpend(ctx context.Context, st store.Usage, runID string) string {
	rows, err := st.RunUsage(context.WithoutCancel(ctx), runID)
	if err != nil || len(rows) == 0 {
		return ""
	}

	tokens, usd, priced := 0, 0.0, false

	for _, row := range rows {
		tokens += row.Total

		if row.CostUSD != nil {
			usd, priced = usd+*row.CostUSD, true
		}
	}

	if priced {
		return fmt.Sprintf("%d tokens · $%.2f", tokens, usd)
	}

	return fmt.Sprintf("%d tokens", tokens)
}
