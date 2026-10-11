package agent

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

func memoryEntryOfLineLen(id int64, lineLen int) store.Memory {
	prefix := fmt.Sprintf("[%d] ", id)

	return store.Memory{ID: id, Text: strings.Repeat("m", lineLen-len(prefix))}
}

// TestThePreloadCapIsExact: each line costs its length plus the newline that joins it, an entry landing exactly on the cap still fits, and the header counts precisely what was left out.
func TestThePreloadCapIsExact(t *testing.T) {
	t.Parallel()

	first := 4000

	exact := renderMemoryPreload([]store.Memory{
		memoryEntryOfLineLen(2, first),
		memoryEntryOfLineLen(1, maxMemoryPreloadBytes-(first+1)),
	}, false)

	if !strings.Contains(exact, "[1] ") || strings.Contains(exact, "older entries") {
		t.Errorf("an entry ending exactly on the cap should be shown with nothing reported hidden:\n%.300s", exact)
	}

	over := renderMemoryPreload([]store.Memory{
		memoryEntryOfLineLen(3, first),
		memoryEntryOfLineLen(2, maxMemoryPreloadBytes-first),
		memoryEntryOfLineLen(1, 10),
	}, false)

	if strings.Contains(over, "[2] ") || strings.Contains(over, "[1] ") {
		t.Errorf("an entry one byte over the cap (newline counted) should end the preload:\n%.300s", over)
	}

	if !strings.Contains(over, " 2 older entries did not fit") {
		t.Errorf("header should count the two entries left out:\n%.300s", over)
	}

	alone := renderMemoryPreload([]store.Memory{memoryEntryOfLineLen(1, maxMemoryPreloadBytes*2)}, false)
	if !strings.Contains(alone, "[1] ") || strings.Contains(alone, "older entries") {
		t.Errorf("the newest entry is shown even when it alone exceeds the cap:\n%.300s", alone)
	}
}
