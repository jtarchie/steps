package web

import (
	"strings"
	"testing"
)

// TestVendoredHtmxStillSupportsWhatTheStreamSends pins the parts of the
// vendored htmx build the live stream's fragments are written against, so a
// bump that drops one fails here rather than in a browser. No Go test can run
// the build; what it can do is refuse to ship fragments the build no longer
// understands.
//
// The shell is the one that needs saying: `hx-morph-skip-children` on the
// NEW element works because the morph syncs attributes onto the old element
// first and only then asks whether that element (now carrying the attribute)
// says to leave its children alone. That order is what lets a fragment ask
// for an attribute-only morph, and was verified against this build in jsdom.
// A build whose check precedes its sync would morph the children against an
// empty fragment — deleting the reader's whole transcript under a container.
func TestVendoredHtmxStillSupportsWhatTheStreamSends(t *testing.T) {
	t.Parallel()

	build, err := assets.ReadFile("static/htmx.min.js")
	if err != nil {
		t.Fatalf("read vendored htmx: %v", err)
	}

	for _, want := range []string{
		`morphSkipChildren:"[hx-morph-skip-children]"`,
		`"outerMorph"===`,
		`"beforeend"===`,
		`"delete"===`,
	} {
		if !strings.Contains(string(build), want) {
			t.Errorf("the vendored htmx no longer carries %s, which the stream's fragments rely on", want)
		}
	}
}
