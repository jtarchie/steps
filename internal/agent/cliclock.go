package agent

import (
	"fmt"
	"time"
)

// cliClockReadings is how many times across a deadline a bridged tool result carries the time: often enough to pace by, rare enough that the model still reads it.
const cliClockReadings = 10

// cliClock is the only clock a CLI agent has. Its system message names a deadline, and nothing can be said to a running child except through a tool result, so the hosted loop's wrap-up nudge never reaches it. Without a reading the model guesses: a reconnaissance agent stopped at 8m of 60m, sure it was out of time.
type cliClock struct {
	start    time.Time
	deadline time.Time
	bucket   int
	warned   bool
}

// reading is the note to append to a tool result at now, or "" when this result carries none. The wrap-up warning rides the first result inside the last fifth, as the hosted loop's does.
func (c *cliClock) reading(now time.Time) string {
	total := c.deadline.Sub(c.start)
	if c.deadline.IsZero() || total <= 0 {
		return ""
	}

	elapsed, left := now.Sub(c.start), c.deadline.Sub(now)
	bucket := int(elapsed * cliClockReadings / total)
	warn := !c.warned && left <= total/timeoutWarningFraction

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
