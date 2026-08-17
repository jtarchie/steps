package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/glamour/ansi"
	"github.com/charmbracelet/glamour/styles"
	"golang.org/x/sys/unix"

	"github.com/jtarchie/steps/docs"
)

// DocsCmd renders the embedded documentation (docs/*.md — the same pages the
// web UI serves at /docs) in the terminal. No page name renders the index,
// which lists the rest.
type DocsCmd struct {
	Page    string `arg:""                                               help:"doc page to read, e.g. resources (defaults to the index)" optional:""`
	NoPager bool   `help:"print to stdout instead of paging long output" name:"no-pager"`
}

// Run renders the requested page through glamour when stdout is a terminal
// (paged when it is long — see pageOutput), and passes the raw markdown
// through when piped — so `steps docs agents | grep` and scripting both stay
// useful.
func (d *DocsCmd) Run() error {
	name := d.Page
	if name == "" {
		name = "README.md"
	}

	if !strings.HasSuffix(name, ".md") {
		name += ".md"
	}

	body, err := docs.Page(name)
	if err != nil {
		return fmt.Errorf("steps docs: %w (available: %s)", err, strings.Join(pageNames(), ", "))
	}

	if !stdoutIsTerminal() {
		fmt.Print(string(body))

		return nil
	}

	renderer, err := glamour.NewTermRenderer(
		glamour.WithStyles(docsTermStyle()),
		glamour.WithWordWrap(docsWrapWidth()),
	)
	if err != nil {
		return fmt.Errorf("steps docs: could not build renderer: %w", err)
	}

	rendered, err := renderer.Render(string(body))
	if err != nil {
		return fmt.Errorf("steps docs: could not render %s: %w", name, err)
	}

	if !d.NoPager {
		err = pageOutput(rendered)
		if err == nil {
			return nil
		}
		// A missing or failing pager degrades to plain printing, never to a
		// lost page.
	}

	fmt.Print(rendered)

	return nil
}

// docsTermStyle is glamour's dark config re-dressed in the product's own
// palette (app.css's :root) — the same substitution the web UI makes for its
// code blocks. The stock dark style's yellow-on-purple H1 pill and mauve
// code belong to another product; here headings are green like every other
// heading this tool prints, and YAML keys are blue.
func docsTermStyle() ansi.StyleConfig {
	const (
		fg      = "#d8d5c9"
		dim     = "#83887b"
		green   = "#84c06d"
		yellow  = "#d9a94a"
		blue    = "#7aa4d9"
		magenta = "#b98fcc"
		cyan    = "#6fbcb4"
	)

	style := styles.DarkStyleConfig

	style.Document.Color = strPtr(fg)

	style.H1.Color = strPtr(green)
	style.H1.BackgroundColor = nil
	style.H1.Prefix = "# "
	style.H1.Suffix = ""
	style.H2.Color = strPtr(green)
	style.H3.Color = strPtr(green)
	style.H4.Color = strPtr(green)

	style.Link.Color = strPtr(blue)
	style.LinkText.Color = strPtr(cyan)

	style.Code.Color = strPtr(yellow)
	style.Code.BackgroundColor = nil

	style.CodeBlock.Chroma = &ansi.Chroma{
		Text:          ansi.StylePrimitive{Color: strPtr(fg)},
		Comment:       ansi.StylePrimitive{Color: strPtr(dim)},
		Keyword:       ansi.StylePrimitive{Color: strPtr(magenta)},
		KeywordType:   ansi.StylePrimitive{Color: strPtr(cyan)},
		Name:          ansi.StylePrimitive{Color: strPtr(fg)},
		NameTag:       ansi.StylePrimitive{Color: strPtr(blue)},
		NameAttribute: ansi.StylePrimitive{Color: strPtr(blue)},
		NameFunction:  ansi.StylePrimitive{Color: strPtr(cyan)},
		NameConstant:  ansi.StylePrimitive{Color: strPtr(yellow)},
		Literal:       ansi.StylePrimitive{Color: strPtr(green)},
		LiteralString: ansi.StylePrimitive{Color: strPtr(green)},
		LiteralNumber: ansi.StylePrimitive{Color: strPtr(yellow)},
		Operator:      ansi.StylePrimitive{Color: strPtr(dim)},
		Punctuation:   ansi.StylePrimitive{Color: strPtr(dim)},
		Error:         ansi.StylePrimitive{Color: strPtr(fg)},
	}

	return style
}

func strPtr(s string) *string { return &s }

// docsWrapWidth is the render width: the terminal's own, capped at 100 so a
// full-screen terminal still gets a readable measure. The previous hardcoded
// 100 sheared every line in an 80-column pane.
func docsWrapWidth() int {
	const maxWidth = 100

	//nolint:gosec // a file descriptor is a small non-negative int; Fd() only returns uintptr
	size, err := unix.IoctlGetWinsize(int(os.Stdout.Fd()), unix.TIOCGWINSZ)
	if err != nil || size.Col == 0 {
		return maxWidth
	}

	return min(int(size.Col), maxWidth)
}

// pageOutput hands rendered text to the user's pager, the way git does. The
// default is less with -F (quit immediately when it fits one screen, so
// short pages behave exactly as if unpaged), -R (pass ANSI colors), and -X
// (no screen clear on exit, so the page stays in scrollback).
func pageOutput(text string) error {
	pager := os.Getenv("PAGER")
	if pager == "" {
		pager = "less"
	}

	// Via the shell so PAGER="less -R" and friends work verbatim.
	cmd := exec.Command("sh", "-c", pager) //nolint:gosec,noctx // PAGER is the operator's own environment, and an interactive pager outlives any request context
	cmd.Env = append(os.Environ(), "LESS=FRX")
	cmd.Stdin = strings.NewReader(text)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("pager: %w", err)
	}

	return nil
}

// pageNames lists the readable pages without their .md suffix, the way the
// argument is typed.
func pageNames() []string {
	names := make([]string, 0, len(docs.Pages()))
	for _, page := range docs.Pages() {
		names = append(names, strings.TrimSuffix(page, ".md"))
	}

	return names
}

// stdoutIsTerminal mirrors wantNoColor's stderr check for stdout: styled
// output belongs on a live terminal, raw markdown everywhere else.
func stdoutIsTerminal() bool {
	info, err := os.Stdout.Stat()
	if err != nil {
		return false
	}

	return info.Mode()&os.ModeCharDevice != 0
}
