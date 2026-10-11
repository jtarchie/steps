package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestLooksBinaryWithALeadingNUL(t *testing.T) {
	t.Parallel()

	if !looksBinary([]byte("\x00\xff\xfe")) {
		t.Error("a NUL at offset 0 beside invalid UTF-8 was read as text")
	}
}

func TestWebPWithAZeroSideHasNoDimensions(t *testing.T) {
	t.Parallel()

	for _, size := range [][2]int{{0, 5}, {5, 0}} {
		if w, h, ok := imageDimensions("image/webp", webpHeader("VP8 ", size[0], size[1])); ok {
			t.Errorf("a %dx%d WebP decoded as %dx%d", size[0], size[1], w, h)
		}
	}

	if _, _, ok := webpDimensions(webpHeader("VP8 ", 7, 5)[:25]); ok {
		t.Error("a WebP cut off inside its first chunk decoded")
	}
}

// webpDimensions hands back a zero-sided header's raw sides alongside !ok, and the blind description must still not print them.
func TestReadFileDescribesAZeroSidedImageWithoutSides(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeBytes(t, dir, "w0.webp", webpHeader("VP8 ", 0, 5))
	writeBytes(t, dir, "h0.webp", webpHeader("VP8 ", 5, 0))

	for _, name := range []string{"w0.webp", "h0.webp"} {
		got := execReadFile(context.Background(), map[string]any{"path": name}, sightedEnv(dir, imageSight{}))

		content, _ := got["content"].(string)
		if !strings.Contains(content, "WebP image, 30 B") {
			t.Errorf("%s: content = %q, want no dimensions named", name, content)
		}
	}
}

func readimageGIFHeader(width, height byte) []byte {
	return []byte{'G', 'I', 'F', '8', '9', 'a', width, 0, height, 0, 0, 0, 0, ';'}
}

func TestReadFileRefusesAZeroSidedGIF(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeBytes(t, dir, "w0.gif", readimageGIFHeader(0, 5))
	writeBytes(t, dir, "h0.gif", readimageGIFHeader(5, 0))

	for _, name := range []string{"w0.gif", "h0.gif"} {
		got := execReadFile(context.Background(), map[string]any{"path": name}, sightedEnv(dir, hostedSight))
		if got[imageKey] != nil || !strings.Contains(fmt.Sprint(got["content"]), "does not decode") {
			t.Errorf("%s = %v, want it described as undecodable, not sent", name, got)
		}
	}
}
