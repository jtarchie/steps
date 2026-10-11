package agent

import (
	"strings"
	"testing"
)

// A surplus and a shortfall ask the operator for different fixes, so each must arrive with its own sentence.
func TestCLIToolSurfaceNamesWhichWayItDiffers(t *testing.T) {
	t.Parallel()

	missing := checkCLIToolSurface(nil, []string{"mcp__steps__read_file"})
	if missing == nil || !strings.Contains(missing.Error(), `did not report granted tool(s) "mcp__steps__read_file"`) {
		t.Errorf("a shortfall reads %v, want it to name the granted tool the cli did not report", missing)
	}

	surplus := checkCLIToolSurface([]string{"Bash"}, nil)
	if surplus == nil || !strings.Contains(surplus.Error(), `"Bash" beyond the empty grant`) {
		t.Errorf("a surplus over no grant reads %v, want it to name the tool and the empty grant", surplus)
	}
}
