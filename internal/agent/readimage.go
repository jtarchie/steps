package agent

// read_file on a file that is not text: an image the model can be shown goes
// back as an image, and anything else is described in words instead of being
// handed over as bytes the model would read as text.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"

	"google.golang.org/genai"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/shell"
)

// sniffBytes is how much of a file's head decides whether it is text: git's
// window for the same question.
const sniffBytes = 8000

// hostedImageMaxBytes is the largest image a hosted model is sent. Anthropic
// refuses an image over 5 MiB and measures the BASE64, which is a third larger
// than the file, so 3.75 MiB of file is the 5 MiB it allows. Nothing here
// downsamples (that would need an image codec this module does not carry), so
// the provider's limit is the one to meet.
const hostedImageMaxBytes = (5 << 20) * 3 / 4

// hostedImageMaxEdge is Anthropic's per-image pixel ceiling: a side over 8000
// is refused outright rather than scaled.
const hostedImageMaxEdge = 8000

// cliImageMaxBytes bounds what the bridge hands a CLI. Looser than the hosted
// cap because Claude Code downsamples an MCP image itself (verified against
// 2.1.291: an MCP image result goes through the resize-and-byte-budget pass its
// own Read tool uses), so the provider's limits are its to meet. What this
// bounds is the transfer, and it stays under maxScriptOutputBytes so a
// container's read of the file can never come back cut short.
const cliImageMaxBytes = 10 << 20

// imageSight is what the model behind a conversation can do with an image a
// tool read. The zero value is a model that cannot be shown one.
type imageSight struct {
	maxBytes int64
	// maxEdge is the longest side the model accepts, 0 for no ceiling.
	maxEdge int
}

func (s imageSight) sees() bool { return s.maxBytes > 0 }

// sightFor answers for one source. A CLI is asked nothing about its model: it
// receives an MCP image and decides for itself.
func sightFor(ri config.ResolvedInvocation) imageSight {
	if ri.CLI != "" {
		return imageSight{maxBytes: cliImageMaxBytes}
	}

	if config.ModelSeesImages(ri.ModelName) {
		return imageSight{maxBytes: hostedImageMaxBytes, maxEdge: hostedImageMaxEdge}
	}

	return imageSight{}
}

// imageKey is where a read_file result carries its toolImage. Only that typed
// value counts, so a tool returning a JSON "image" field is never mistaken for one.
const imageKey = "image"

// toolImage is an image on its way to the model. Every rendering of it as data —
// the tool message's JSON, a transcript, a debug line, fmt's %v in compaction's
// estimate — is the metadata and a digest, never the bytes: the bytes leave
// this process only as an image part, which is what keeps them out of the
// state database.
type toolImage struct {
	path   string
	mime   string
	data   []byte
	width  int
	height int
	sum    string
}

func newToolImage(path, mime string, data []byte, width, height int) toolImage {
	sum := sha256.Sum256(data)

	return toolImage{path: path, mime: mime, data: data, width: width, height: height, sum: hex.EncodeToString(sum[:])}
}

func (i toolImage) MarshalJSON() ([]byte, error) {
	//nolint:wrapcheck // a map of strings and ints cannot fail to encode
	return json.Marshal(map[string]any{
		"mime_type": i.mime,
		"bytes":     len(i.data),
		"width":     i.width,
		"height":    i.height,
		"sha256":    i.sum,
	})
}

func (i toolImage) String() string {
	return fmt.Sprintf("[%s %dx%d, %s, sha256 %s]", i.mime, i.width, i.height, shell.FormatBytes(len(i.data)), i.sum)
}

// sniffImage names the image formats every image-taking provider accepts, from
// the bytes rather than the extension, and "" for anything else.
func sniffImage(head []byte) string {
	switch mime := http.DetectContentType(head); mime {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return mime
	default:
		return ""
	}
}

// looksBinary asks for a NUL AND invalid UTF-8, not either alone. A NUL by
// itself is legitimate text — `find -print0` output spilled to a file is
// exactly that — and invalid UTF-8 by itself is a Latin-1 file today's
// read_file shows readably enough. Both together is bytes no model can read.
func looksBinary(head []byte) bool {
	if bytes.IndexByte(head, 0) < 0 {
		return false
	}

	// A head cut mid-rune is still text: the cut is ours, not the file's.
	for i := len(head) - 1; i >= max(0, len(head)-utf8.UTFMax); i-- {
		if utf8.RuneStart(head[i]) {
			if !utf8.FullRune(head[i:]) {
				head = head[:i]
			}

			break
		}
	}

	return !utf8.Valid(head)
}

// isNonText reports whether read_file must answer for head's file with
// something other than its text.
func isNonText(head []byte) bool {
	head = head[:min(len(head), sniffBytes)]

	return sniffImage(head) != "" || looksBinary(head)
}

// nonTextResult is read_file's answer for a file isNonText picked out: the
// image itself, or a description. read is however much of the file the caller
// already has; the rest is fetched only for an image the model will actually
// be sent.
func nonTextResult(ctx context.Context, files tree, resolved, rel string, read []byte, size int64, sight imageSight) map[string]any {
	head := read[:min(len(read), sniffBytes)]

	mime := sniffImage(head)
	if mime == "" {
		return map[string]any{"content": fmt.Sprintf(
			"%s: binary file, %s (%s). read_file returns text, so its bytes are not shown.",
			rel, shell.FormatBytes(int(size)), mimeEssence(http.DetectContentType(head)))}
	}

	width, height, _ := imageDimensions(mime, head)

	if !sight.sees() {
		return map[string]any{"content": fmt.Sprintf(
			"%s: %s. This model can't be shown images, so read_file describes the file instead of returning its bytes.",
			rel, describeImage(mime, size, width, height))}
	}

	if size > sight.maxBytes {
		return map[string]any{"error": fmt.Sprintf(
			"read_file: %s is a %s, over the %s an image can be sent to this model at. Make a smaller copy (scaled down or cropped) and read that instead.",
			rel, describeImage(mime, size, width, height), shell.FormatBytes(int(sight.maxBytes)))}
	}

	data := read

	if int64(len(read)) < size {
		var err error

		data, err = files.readBytes(ctx, resolved, sight.maxBytes)
		if err != nil {
			return map[string]any{"error": err.Error()}
		}
	}

	if int64(len(data)) != size {
		return map[string]any{"error": fmt.Sprintf("read_file: %s changed while it was being read (%d of %d bytes)", rel, len(data), size)}
	}

	return imageResult(rel, mime, data, sight)
}

// imageResult checks a whole image against what the model takes and packages
// it. A header that will not decode is described rather than sent: the
// provider would refuse it, and a refused request fails the step rather than
// the call.
func imageResult(rel, mime string, data []byte, sight imageSight) map[string]any {
	size := int64(len(data))

	width, height, ok := imageDimensions(mime, data)
	if !ok {
		return map[string]any{"content": fmt.Sprintf(
			"%s: %s whose header does not decode, so it was not sent as an image.", rel, describeImage(mime, size, 0, 0))}
	}

	if sight.maxEdge > 0 && max(width, height) > sight.maxEdge {
		return map[string]any{"error": fmt.Sprintf(
			"read_file: %s is a %s, over the %d-pixel side an image can be sent to this model at. Make a scaled-down copy and read that instead.",
			rel, describeImage(mime, size, width, height), sight.maxEdge)}
	}

	return map[string]any{
		"content": fmt.Sprintf("%s: %s, attached as an image.", rel, describeImage(mime, size, width, height)),
		imageKey:  newToolImage(rel, mime, data, width, height),
	}
}

// describeImage is the one spelling of an image in words, so a description, a
// refusal and a placeholder all name it alike.
func describeImage(mime string, size int64, width, height int) string {
	name := strings.ToUpper(strings.TrimPrefix(mime, "image/"))
	if name == "WEBP" {
		name = "WebP"
	}

	if width > 0 && height > 0 {
		return fmt.Sprintf("%s image, %dx%d, %s", name, width, height, shell.FormatBytes(int(size)))
	}

	return fmt.Sprintf("%s image, %s", name, shell.FormatBytes(int(size)))
}

func mimeEssence(contentType string) string {
	essence, _, _ := strings.Cut(contentType, ";")

	return essence
}

// imageDimensions reads only the header. WebP is parsed by hand because the
// standard library has no decoder for it.
func imageDimensions(mime string, data []byte) (width, height int, ok bool) {
	var (
		cfg image.Config
		err error
	)

	switch mime {
	case "image/png":
		cfg, err = png.DecodeConfig(bytes.NewReader(data))
	case "image/jpeg":
		cfg, err = jpeg.DecodeConfig(bytes.NewReader(data))
	case "image/gif":
		cfg, err = gif.DecodeConfig(bytes.NewReader(data))
	case "image/webp":
		return webpDimensions(data)
	default:
		return 0, 0, false
	}

	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return 0, 0, false
	}

	return cfg.Width, cfg.Height, true
}

// webpDimensions reads the first chunk after the RIFF header, in whichever of
// the three encodings (lossy VP8, lossless VP8L, extended VP8X) the file uses.
func webpDimensions(data []byte) (width, height int, ok bool) {
	const chunk = 20 // "RIFF" size "WEBP" fourcc size

	if len(data) < chunk+10 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		return 0, 0, false
	}

	body := data[chunk:]

	switch string(data[12:16]) {
	case "VP8 ":
		width, height = vp8Dimensions(body)
	case "VP8L":
		width, height = vp8lDimensions(body)
	case "VP8X":
		width, height = littleEndian24(body[4:7])+1, littleEndian24(body[7:10])+1
	}

	return width, height, width > 0 && height > 0
}

// vp8Dimensions is a lossy frame's size, after its three-byte start code.
func vp8Dimensions(body []byte) (width, height int) {
	if body[3] != 0x9d || body[4] != 0x01 || body[5] != 0x2a {
		return 0, 0
	}

	return int(binary.LittleEndian.Uint16(body[6:8]) & 0x3fff), int(binary.LittleEndian.Uint16(body[8:10]) & 0x3fff)
}

// vp8lDimensions is a lossless frame's size: two 14-bit fields, each one less
// than the side it names.
func vp8lDimensions(body []byte) (width, height int) {
	if body[0] != 0x2f {
		return 0, 0
	}

	bits := binary.LittleEndian.Uint32(body[1:5])

	return int(bits&0x3fff) + 1, int((bits>>14)&0x3fff) + 1
}

func littleEndian24(b []byte) int {
	return int(b[0]) | int(b[1])<<8 | int(b[2])<<16
}

// withImageParts is a tool turn's results as the model receives them: each
// image a result carries rides after the results as an image part. Chat
// Completions has no image in a tool message, and the adapter turns these
// parts into a user message following the tool messages — the shape OpenAI,
// Anthropic's compatibility layer and OpenRouter all accept. The adapter puts a
// user message's text ahead of its images, so one line names them all, in order.
func withImageParts(parts []*genai.Part) []*genai.Part {
	var (
		paths  []string
		images []*genai.Part
	)

	for _, part := range parts {
		if part.FunctionResponse == nil {
			continue
		}

		img, ok := part.FunctionResponse.Response[imageKey].(toolImage)
		if !ok {
			continue
		}

		paths = append(paths, img.path)
		images = append(images, &genai.Part{InlineData: &genai.Blob{MIMEType: img.mime, Data: img.data}})
	}

	if len(images) == 0 {
		return parts
	}

	label := &genai.Part{Text: "The images read_file returned, in the order it was called: " + strings.Join(paths, ", ")}

	return append(append(slices.Clone(parts), label), images...)
}

// withoutImages is a conversation carried to a source that cannot be shown
// images — a fallback: model, resuming what a sighted one began. The images it
// already saw would otherwise be sent to an endpoint that refuses the request.
func withoutImages(contents []*genai.Content) []*genai.Content {
	out := make([]*genai.Content, len(contents))

	for i, content := range contents {
		out[i] = content

		if content == nil || !slices.ContainsFunc(content.Parts, func(p *genai.Part) bool { return p != nil && p.InlineData != nil }) {
			continue
		}

		stripped := *content
		stripped.Parts = make([]*genai.Part, len(content.Parts))

		for j, part := range content.Parts {
			stripped.Parts[j] = part
			if part != nil && part.InlineData != nil {
				stripped.Parts[j] = &genai.Part{Text: "[image omitted: this model can't be shown images]"}
			}
		}

		out[i] = &stripped
	}

	return out
}
