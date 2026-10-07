package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/jtarchie/steps/internal/config"
)

// hostedSight is what a hosted model known to take images gets.
var hostedSight = imageSight{maxBytes: hostedImageMaxBytes, maxEdge: hostedImageMaxEdge} //nolint:gochecknoglobals // read-only test fixture

func encodedImage(t *testing.T, format string, width, height int) []byte {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for x := range width {
		for y := range height {
			img.Set(x, y, color.RGBA{R: 200, G: uint8(x), B: uint8(y), A: 255})
		}
	}

	var (
		buf bytes.Buffer
		err error
	)

	switch format {
	case "png":
		err = png.Encode(&buf, img)
	case "jpeg":
		err = jpeg.Encode(&buf, img, nil)
	case "gif":
		err = gif.Encode(&buf, img, nil)
	default:
		t.Fatalf("no encoder for %s", format)
	}

	if err != nil {
		t.Fatal(err)
	}

	return buf.Bytes()
}

// webpHeader builds the first chunk of a WebP in one of its three encodings —
// enough for the sniffer and the dimension parser, which is all read_file asks
// of a WebP. There is no WebP encoder in the standard library to make a whole one.
func webpHeader(chunk string, width, height int) []byte {
	body := make([]byte, 10)

	switch chunk {
	case "VP8 ":
		copy(body[3:6], []byte{0x9d, 0x01, 0x2a})
		binary.LittleEndian.PutUint16(body[6:8], uint16(width))   //nolint:gosec // small test dimensions
		binary.LittleEndian.PutUint16(body[8:10], uint16(height)) //nolint:gosec // small test dimensions
	case "VP8L":
		body[0] = 0x2f
		binary.LittleEndian.PutUint32(body[1:5], uint32(width-1)|uint32(height-1)<<14) //nolint:gosec // small test dimensions
	case "VP8X":
		w, h := width-1, height-1
		copy(body[4:7], []byte{byte(w), byte(w >> 8), byte(w >> 16)})  //nolint:gosec // 24-bit fields, taken a byte at a time
		copy(body[7:10], []byte{byte(h), byte(h >> 8), byte(h >> 16)}) //nolint:gosec // 24-bit fields, taken a byte at a time
	}

	out := []byte("RIFF\x00\x00\x00\x00WEBP" + chunk + "\x0a\x00\x00\x00")

	return append(out, body...)
}

func writeBytes(t *testing.T, dir, name string, data []byte) {
	t.Helper()

	err := os.WriteFile(filepath.Join(dir, name), data, 0o600)
	if err != nil {
		t.Fatal(err)
	}
}

func sightedEnv(dir string, sight imageSight) toolEnv {
	env := testEnv(dir)
	env.sight = sight

	return env
}

func TestSniffImageTrustsBytesNotNames(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		data []byte
		want string
	}{
		{"png", encodedImage(t, "png", 2, 2), "image/png"},
		{"jpeg", encodedImage(t, "jpeg", 2, 2), "image/jpeg"},
		{"gif", encodedImage(t, "gif", 2, 2), "image/gif"},
		{"webp", webpHeader("VP8L", 2, 2), "image/webp"},
		{"text", []byte("just words"), ""},
		{"bmp", []byte("BM\x00\x00\x00\x00\x00\x00\x00\x00"), ""},
	} {
		got := sniffImage(tc.data)
		if got != tc.want {
			t.Errorf("sniffImage(%s) = %q, want %q", tc.name, got, tc.want)
		}
	}

	dir := t.TempDir()
	writeBytes(t, dir, "notes.txt", encodedImage(t, "png", 3, 2))
	writeBytes(t, dir, "shot.png", []byte("not a picture at all"))

	disguised := execReadFile(context.Background(), map[string]any{"path": "notes.txt"}, sightedEnv(dir, hostedSight))
	if _, ok := disguised[imageKey].(toolImage); !ok {
		t.Errorf("a PNG named .txt was not returned as an image: %v", disguised)
	}

	named := execReadFile(context.Background(), map[string]any{"path": "shot.png"}, sightedEnv(dir, hostedSight))
	if named["content"] != "not a picture at all" {
		t.Errorf("a text file named .png was not returned as its text: %v", named)
	}
}

func TestLooksBinaryNeedsBothSignals(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		head []byte
		want bool
	}{
		{"zip", []byte("PK\x03\x04\x14\x00\x00\x00\x08\x00\xff\xfe\x80"), true},
		{"find -print0 output", []byte("./a.go\x00./b.go\x00./naïve.go\x00"), false},
		{"latin-1 text", []byte("caf\xe9 au lait"), false},
		{"plain text", []byte("hello"), false},
		// A head cut inside a multibyte rune is still text: the cut is ours.
		{"utf-8 cut mid-rune", append([]byte("a\x00b é"), "€"[:2]...), false},
	} {
		got := looksBinary(tc.head)
		if got != tc.want {
			t.Errorf("looksBinary(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestImageDimensions(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		mime string
		data []byte
	}{
		{"png", "image/png", encodedImage(t, "png", 7, 5)},
		{"jpeg", "image/jpeg", encodedImage(t, "jpeg", 7, 5)},
		{"gif", "image/gif", encodedImage(t, "gif", 7, 5)},
		{"webp lossy", "image/webp", webpHeader("VP8 ", 7, 5)},
		{"webp lossless", "image/webp", webpHeader("VP8L", 7, 5)},
		{"webp extended", "image/webp", webpHeader("VP8X", 7, 5)},
	} {
		w, h, ok := imageDimensions(tc.mime, tc.data)
		if !ok || w != 7 || h != 5 {
			t.Errorf("imageDimensions(%s) = %dx%d, %v; want 7x5", tc.name, w, h, ok)
		}
	}

	// An extended canvas is 24 bits a side, past what the other two can say.
	if w, h, ok := imageDimensions("image/webp", webpHeader("VP8X", 70000, 5)); !ok || w != 70000 || h != 5 {
		t.Errorf("imageDimensions(webp extended, 70000x5) = %dx%d, %v", w, h, ok)
	}

	_, _, ok := imageDimensions("image/png", []byte("\x89PNG\r\n\x1a\ngarbage that is no IHDR"))
	if ok {
		t.Error("a PNG signature over a broken header decoded")
	}

	lossy := webpHeader("VP8 ", 7, 5)
	lossy[23] = 0 // the frame's start code

	if _, _, ok := imageDimensions("image/webp", lossy); ok {
		t.Error("a lossy WebP frame without its start code decoded")
	}
}

// readSightFixture is one 6x4 PNG, read three ways below: the decision the
// whole feature turns on is the same file put to different models.
func readSightFixture(t *testing.T) (string, []byte) {
	t.Helper()

	dir := t.TempDir()
	data := encodedImage(t, "png", 6, 4)
	writeBytes(t, dir, "shot.png", data)

	return dir, data
}

func TestReadFileShowsASightedModelTheImage(t *testing.T) {
	t.Parallel()

	dir, data := readSightFixture(t)

	got := execReadFile(context.Background(), map[string]any{"path": "shot.png"}, sightedEnv(dir, hostedSight))

	img, ok := got[imageKey].(toolImage)
	if !ok {
		t.Fatalf("no image in the result: %v", got)
	}

	if !bytes.Equal(img.data, data) || img.mime != "image/png" || img.width != 6 || img.height != 4 {
		t.Errorf("image = %s %dx%d, %d bytes; want the file's own image/png 6x4, %d bytes", img.mime, img.width, img.height, len(img.data), len(data))
	}

	if content, _ := got["content"].(string); !strings.Contains(content, "PNG image, 6x4") || !strings.Contains(content, "attached as an image") {
		t.Errorf("content = %q", content)
	}
}

func TestReadFileDescribesAnImageToABlindModel(t *testing.T) {
	t.Parallel()

	dir, _ := readSightFixture(t)

	got := execReadFile(context.Background(), map[string]any{"path": "shot.png"}, sightedEnv(dir, imageSight{}))

	if _, ok := got[imageKey]; ok {
		t.Errorf("an image went to a model that cannot be shown one: %v", got)
	}

	content, _ := got["content"].(string)
	if !strings.Contains(content, "shot.png: PNG image, 6x4") || !strings.Contains(content, "can't be shown images") {
		t.Errorf("content = %q", content)
	}

	if strings.Contains(content, "PNG\r\n") {
		t.Errorf("the description carries the file's raw bytes: %q", content)
	}
}

func TestReadFileIgnoresARangeOnAnImage(t *testing.T) {
	t.Parallel()

	dir, data := readSightFixture(t)

	got := execReadFile(context.Background(), map[string]any{"path": "shot.png", "start_line": float64(2), "end_line": float64(3)},
		sightedEnv(dir, hostedSight))

	if img, ok := got[imageKey].(toolImage); !ok || !bytes.Equal(img.data, data) {
		t.Fatalf("a ranged read of an image did not return the whole image: %v", got)
	}

	if content, _ := got["content"].(string); !strings.Contains(content, "start_line/end_line do not apply") {
		t.Errorf("content = %q, want it to say the range was ignored", content)
	}
}

func TestReadFileImageLimits(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeBytes(t, dir, "wide.png", encodedImage(t, "png", hostedImageMaxEdge+1, 1))
	writeBytes(t, dir, "edge.png", encodedImage(t, "png", hostedImageMaxEdge, 1))
	writeBytes(t, dir, "broken.png", []byte("\x89PNG\r\n\x1a\nnot an IHDR at all, padded out"))

	// Bigger than the limit by one byte: a real PNG whose tail is padding,
	// since the size check is on the file, before anything decodes it.
	big := append(encodedImage(t, "png", 2, 2), make([]byte, hostedImageMaxBytes)...)
	writeBytes(t, dir, "big.png", big[:hostedImageMaxBytes+1])
	writeBytes(t, dir, "fits.png", big[:hostedImageMaxBytes])

	read := func(name string, sight imageSight) map[string]any {
		return execReadFile(context.Background(), map[string]any{"path": name}, sightedEnv(dir, sight))
	}

	if got := read("big.png", hostedSight); got["error"] == nil || !strings.Contains(fmt.Sprint(got["error"]), "Make a smaller copy") {
		t.Errorf("an image over the byte limit = %v, want it refused", got)
	}

	if got := read("fits.png", hostedSight); got[imageKey] == nil {
		t.Errorf("an image exactly at the byte limit = %v, want it sent", got)
	}

	if got := read("wide.png", hostedSight); got["error"] == nil || !strings.Contains(fmt.Sprint(got["error"]), "8000-pixel") {
		t.Errorf("an image over the edge limit = %v, want it refused", got)
	}

	if got := read("edge.png", hostedSight); got[imageKey] == nil {
		t.Errorf("an image exactly at the edge limit = %v, want it sent", got)
	}

	if got := read("broken.png", hostedSight); got[imageKey] != nil || !strings.Contains(fmt.Sprint(got["content"]), "does not decode") {
		t.Errorf("an image whose header does not decode = %v, want it described, not sent", got)
	}
}

// TestReadFileLetsTheCLIScaleForItself: neither hosted refusal applies to a
// CLI, which downsamples what it is handed.
func TestReadFileLetsTheCLIScaleForItself(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeBytes(t, dir, "wide.png", encodedImage(t, "png", hostedImageMaxEdge+1, 1))
	writeBytes(t, dir, "big.png", append(encodedImage(t, "png", 2, 2), make([]byte, hostedImageMaxBytes)...))

	cli := sightedEnv(dir, sightFor(config.ResolvedInvocation{CLI: "claude"}))

	for _, name := range []string{"wide.png", "big.png"} {
		if got := execReadFile(context.Background(), map[string]any{"path": name}, cli); got[imageKey] == nil {
			t.Errorf("a CLI was refused %s, which only a hosted limit refuses: %v", name, got)
		}
	}
}

func TestReadFileDescribesBinaryFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	zip := append([]byte("PK\x03\x04\x14\x00\x00\x00\x08\x00\xff\xfe\x80"), make([]byte, 2000)...)
	writeBytes(t, dir, "bundle.zip", zip)
	writeBytes(t, dir, "files.txt", []byte("./a.go\x00./b.go\x00"))

	for _, args := range []map[string]any{
		{"path": "bundle.zip"},
		{"path": "bundle.zip", "start_line": float64(1)},
	} {
		got := execReadFile(context.Background(), args, testEnv(dir))

		content, _ := got["content"].(string)
		if !strings.Contains(content, "bundle.zip: binary file, 2.0 KB (application/zip)") {
			t.Errorf("read_file(%v) = %q", args, content)
		}
	}

	if got := execReadFile(context.Background(), map[string]any{"path": "files.txt"}, testEnv(dir)); got["content"] != "./a.go\x00./b.go\x00" {
		t.Errorf("NUL-separated text was not returned as text: %v", got)
	}
}

// TestImageNeverRendersItsBytes covers every place a tool result becomes data
// rather than an image: the tool message's JSON, the transcript, the debug log,
// the loop detector's signature and compaction's %v. Any one of them carrying
// the base64 puts the image in the state database or in front of the model as
// text, which is the mojibake this replaced, a third larger.
func TestImageNeverRendersItsBytes(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	data := encodedImage(t, "png", 32, 32)
	writeBytes(t, dir, "shot.png", data)

	result := execReadFile(context.Background(), map[string]any{"path": "shot.png"}, sightedEnv(dir, hostedSight))
	encoded := base64.StdEncoding.EncodeToString(data)

	for name, rendered := range map[string]string{
		"transcript": renderResultContent(result),
		"debug log":  debugToolResultPreview(result),
		"%v":         fmt.Sprintf("%v", result),
	} {
		if strings.Contains(rendered, encoded[:40]) || strings.Contains(rendered, string(data[8:24])) {
			t.Errorf("the %s rendering carries the image's bytes: %.200s", name, rendered)
		}

		if !strings.Contains(rendered, "image/png") {
			t.Errorf("the %s rendering does not name the image: %.200s", name, rendered)
		}
	}

	if len(renderResultContent(result)) > 1000 {
		t.Errorf("the transcript rendering is %d bytes for a %d-byte image", len(renderResultContent(result)), len(data))
	}
}

func TestWithImagePartsFollowsTheResults(t *testing.T) {
	t.Parallel()

	first := newToolImage("a.png", "image/png", []byte("A"), 1, 1)
	second := newToolImage("b.gif", "image/gif", []byte("B"), 1, 1)

	parts := []*genai.Part{
		{FunctionResponse: &genai.FunctionResponse{Name: "read_file", Response: map[string]any{imageKey: first}}},
		{FunctionResponse: &genai.FunctionResponse{Name: "list_dir", Response: map[string]any{"entries": []any{}}}},
		{FunctionResponse: &genai.FunctionResponse{Name: "read_file", Response: map[string]any{imageKey: second}}},
	}

	got := withImageParts(parts)

	if len(got) != 6 || got[3].Text == "" || got[4].InlineData == nil || got[5].InlineData == nil {
		t.Fatalf("parts = %d, want the 3 results, then one label, then 2 images", len(got))
	}

	if !strings.Contains(got[3].Text, "a.png, b.gif") {
		t.Errorf("label = %q, want it to name the images in call order", got[3].Text)
	}

	if string(got[4].InlineData.Data) != "A" || got[5].InlineData.MIMEType != "image/gif" {
		t.Error("the images are not in call order")
	}

	if len(parts) != 3 {
		t.Error("the caller's slice was grown in place")
	}

	plain := parts[1:2]
	if again := withImageParts(plain); len(again) != 1 {
		t.Errorf("a turn with no image grew to %d parts", len(again))
	}
}

// TestResumeOnABlindSourceDropsTheImages is the failover seam: a conversation
// a sighted source began carries image parts, and a fallback that cannot be
// shown images would have its endpoint refuse the whole request.
func TestResumeOnABlindSourceDropsTheImages(t *testing.T) {
	t.Parallel()

	contents := []*genai.Content{
		{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "look"}}},
		{Role: genai.RoleUser, Parts: withImageParts([]*genai.Part{{FunctionResponse: &genai.FunctionResponse{
			Name: "read_file", Response: map[string]any{imageKey: newToolImage("a.png", "image/png", []byte("A"), 1, 1)},
		}}})},
	}

	hasImage := func(req *model.LLMRequest) bool {
		for _, content := range req.Contents {
			for _, part := range content.Parts {
				if part.InlineData != nil {
					return true
				}
			}
		}

		return false
	}

	conv := agentConversation{resume: &resumeCheckpoint{contents: contents}}

	conv.env.sight = hostedSight
	if !hasImage(buildAgentRequest(conv)) {
		t.Error("a sighted fallback lost the images it can be shown")
	}

	conv.env.sight = imageSight{}

	blind := buildAgentRequest(conv)
	if hasImage(blind) {
		t.Error("a blind fallback was sent an image")
	}

	if !strings.Contains(blind.Contents[1].Parts[2].Text, "image omitted") {
		t.Errorf("the image was dropped without a note: %+v", blind.Contents[1].Parts)
	}

	if contents[1].Parts[2].InlineData == nil {
		t.Error("stripping rewrote the checkpoint it was handed")
	}
}

func TestSightFor(t *testing.T) {
	t.Parallel()

	if got := sightFor(config.ResolvedInvocation{CLI: "claude", ModelName: "sonnet"}); got.maxBytes != cliImageMaxBytes || got.maxEdge != 0 {
		t.Errorf("a CLI source = %+v", got)
	}

	if got := sightFor(config.ResolvedInvocation{ModelName: "anthropic/claude-sonnet-4.5"}); got != hostedSight {
		t.Errorf("a hosted claude = %+v", got)
	}

	if got := sightFor(config.ResolvedInvocation{ModelName: "qwen/qwen3.7-flash"}); got.sees() {
		t.Errorf("a hosted model not known to take images = %+v", got)
	}
}

func TestEstimatePartTokensPricesAnImageAsAnImage(t *testing.T) {
	t.Parallel()

	part := &genai.Part{InlineData: &genai.Blob{MIMEType: "image/png", Data: make([]byte, 2<<20)}}
	if got := estimatePartTokens(part); got != imageTokenEstimate {
		t.Errorf("estimatePartTokens(a 2 MB image) = %d, want %d", got, imageTokenEstimate)
	}
}

// TestContextPathImage is the preparation-time read: bounded by the image
// limit, not by max_context_bytes:, which budgets text.
func TestContextPathImage(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	data := encodedImage(t, "png", 40, 30)
	writeBytes(t, dir, "shot.png", data)

	blocks, err := loadContextBlocks(t.Context(), dir, []string{"shot.png"}, 100, hostedSight)
	if err != nil {
		t.Fatal(err)
	}

	if blocks[0].image == nil || !bytes.Equal(blocks[0].image.data, data) {
		t.Fatalf("block = %+v, want the whole image despite a 100-byte text budget", blocks[0])
	}
}

// TestContextPathImageIsNeverAPreparationError: a blind model's image and an
// oversized one are both facts about this run's input, delivered as text.
func TestContextPathImageIsNeverAPreparationError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeBytes(t, dir, "shot.png", encodedImage(t, "png", 40, 30))
	writeBytes(t, dir, "huge.png", append(encodedImage(t, "png", 2, 2), make([]byte, hostedImageMaxBytes)...))

	blind, err := loadContextBlocks(t.Context(), dir, []string{"shot.png"}, 0, imageSight{})
	if err != nil {
		t.Fatal(err)
	}

	if blind[0].image != nil || !strings.Contains(blind[0].content, "can't be shown images") {
		t.Errorf("a blind model's block = %+v", blind[0])
	}

	huge, err := loadContextBlocks(t.Context(), dir, []string{"huge.png"}, 0, hostedSight)
	if err != nil {
		t.Fatalf("an oversized image failed preparation: %v", err)
	}

	if huge[0].image != nil || !strings.Contains(huge[0].content, "Make a smaller copy") {
		t.Errorf("an oversized image's block = %+v, want the refusal as its content", huge[0])
	}
}

// TestCLIPromptPointsAtReadFileForAnImage: a prompt is text, so the image is
// named there and fetched through the bridge.
func TestCLIPromptPointsAtReadFileForAnImage(t *testing.T) {
	t.Parallel()

	img := newToolImage("shot.png", "image/png", encodedImage(t, "png", 40, 30), 40, 30)

	prompt := renderCLIPrompt(agentConversation{
		messages:      []string{"What is in it?"},
		contextBlocks: []contextBlock{{path: "shot.png", content: "described", image: &img}},
	})
	if !strings.Contains(prompt, "shot.png: PNG image, 40x30") || !strings.Contains(prompt, "read_file on this path returns the image") {
		t.Errorf("a CLI prompt did not point at read_file for the image: %s", prompt)
	}

	if strings.Contains(prompt, "described") {
		t.Errorf("a CLI prompt inlined the hosted description, which promises an attachment it does not have: %s", prompt)
	}
}

// shrinkingTree is a file that is shorter when read than when it was sized —
// rewritten in between, the way a screenshot being saved is.
type shrinkingTree struct{ hostTree }

func (s shrinkingTree) readBytes(ctx context.Context, path string, limit int64) ([]byte, error) {
	data, err := s.hostTree.readBytes(ctx, path, limit)

	return data[:len(data)/2], err
}

func TestReadFileRefusesAnImageThatChangedUnderIt(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	data := encodedImage(t, "png", 64, 64)
	writeBytes(t, dir, "shot.png", data)

	tree := shrinkingTree{hostTree{dir: dir}}

	got := nonTextResult(context.Background(), tree, filepath.Join(dir, "shot.png"), "shot.png", data[:16], int64(len(data)), hostedSight)
	if got[imageKey] != nil || !strings.Contains(fmt.Sprint(got["error"]), "changed while it was being read") {
		t.Errorf("a half-read image = %v, want it refused rather than sent cut short", got)
	}
}

// TestCLIStreamCarriesAnEchoedImage: the CLI echoes a tool result's image back
// on its own stream, base64 and all, and a line the scanner cannot hold ends
// the step as a stream error after the work was done.
func TestCLIStreamCarriesAnEchoedImage(t *testing.T) {
	t.Parallel()

	payload := strings.Repeat("A", 12<<20) // two hosted-limit images' worth, in one turn
	stream := `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":[` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + payload + `"}}]}]}}` + "\n" +
		`{"type":"result","subtype":"success","result":"done","num_turns":1}` + "\n"

	got, err := parseCLIStream(strings.NewReader(stream), nil, nil)
	if err != nil {
		t.Fatalf("parseCLIStream: %v", err)
	}

	if !got.sawResult {
		t.Error("the result after the image line was never read")
	}
}
