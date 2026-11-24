package agent

import (
	"fmt"
	"time"
)

// clockReadings is how many times across a deadline the model is told the time: often enough to pace by, rare enough that it still reads it.
const clockReadings = 10

// deadlineClock is the only clock an agent has. Its system message names a deadline, and without a reading the model guesses: a reconnaissance agent stopped at 8m of 60m, sure it was out of time. A CLI agent gets it on a tool result, the one thing that can be said to a running child; a hosted one as a message ahead of its next request.
type deadlineClock struct {
	start    time.Time
	deadline time.Time
	bucket   int
	// warned is the whole wrap-up nudge's latch, not only the deadline's: the hosted loop sets it for a turn or token warning too.
	warned bool
	// slowest is the longest model request this attempt has made. Only the hosted loop times one.
	slowest time.Duration
}

// reading is the note to give the model at now, or "" when there is none. The wrap-up warning rides the first reading timeoutWarningDue allows.
func (c *deadlineClock) reading(now time.Time) string {
	total := c.deadline.Sub(c.start)
	if c.deadline.IsZero() || total <= 0 {
		return ""
	}

	elapsed, left := now.Sub(c.start), c.deadline.Sub(now)
	// Capped: past the deadline there is nothing new to say.
	bucket := min(int(elapsed*clockReadings/total), clockReadings)
	warn := !c.warned && timeoutWarningDue(total, left, c.slowest)

	if !warn && bucket == c.bucket {
		return ""
	}

	c.bucket = bucket

	note := fmt.Sprintf("[clock: %s elapsed, %s left before this step's deadline]", elapsed.Round(time.Second), max(left, 0).Round(time.Second))
	if warn {
		c.warned = true
		note += " " + timeoutWarningText
	}

	return note
}
