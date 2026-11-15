package cli

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/jtarchie/steps/docs"
)

// DocsCmd renders the embedded documentation (docs/*.md — the same pages the
// web UI serves at /docs) in the terminal. No page name renders the index,
// which lists the rest.
type DocsCmd struct {
	Page    string `arg:""                                               help:"doc page to read, e.g. resources (defaults to the index)" optional:""`
	NoPager bool   `help:"print to stdout instead of paging long output" name:"no-pager"`
}

// Run prints the requested page's markdown, paged when stdout is a terminal
// (see pageOutput). Raw rather than styled: a terminal markdown renderer
// linked a second chroma and goldmark beside the web UI's, and their lexer
// and style registries were half of every invocation's startup heap.
func (d *DocsCmd) Run(streams Streams) error {
	out := streams.Out

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

	page := string(body)

	if isTerminal(out) && !d.NoPager {
		err = pageOutput(out, streams.Err, page)
		if err == nil {
			return nil
		}
		// A missing or failing pager degrades to plain printing, never to a
		// lost page.
	}

	_, _ = fmt.Fprint(out, page)

	return nil
}

// pageOutput hands text to the user's pager, the way git does. The
// default is less with -F (quit immediately when it fits one screen, so
// short pages behave exactly as if unpaged), -R (pass ANSI colors), and -X
// (no screen clear on exit, so the page stays in scrollback).
func pageOutput(out, errOut io.Writer, text string) error {
	pager := os.Getenv("PAGER")
	if pager == "" {
		pager = "less"
	}

	// Via the shell so PAGER="less -R" and friends work verbatim.
	cmd := exec.Command("sh", "-c", pager) //nolint:gosec,noctx // PAGER is the operator's own environment, and an interactive pager outlives any request context
	cmd.Env = append(os.Environ(), "LESS=FRX")
	cmd.Stdin = strings.NewReader(text)
	cmd.Stdout = out
	cmd.Stderr = errOut

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

// isTerminal mirrors wantNoColor's stderr check for the stream a command writes to (a buffer handed to RunWith is never one): a pager and
// live progress belong on a live terminal, plain output everywhere else.
func isTerminal(out io.Writer) bool {
	file, ok := out.(*os.File)
	if !ok {
		return false
	}

	info, err := file.Stat()
	if err != nil {
		return false
	}

	return info.Mode()&os.ModeCharDevice != 0
}
