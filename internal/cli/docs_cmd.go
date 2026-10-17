package cli

import (
	"fmt"
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

	page := string(body)

	if stdoutIsTerminal() && !d.NoPager {
		err = pageOutput(page)
		if err == nil {
			return nil
		}
		// A missing or failing pager degrades to plain printing, never to a
		// lost page.
	}

	fmt.Print(page)

	return nil
}

// pageOutput hands text to the user's pager, the way git does. The
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

// stdoutIsTerminal mirrors wantNoColor's stderr check for stdout: a pager and
// live progress belong on a live terminal, plain output everywhere else.
func stdoutIsTerminal() bool {
	info, err := os.Stdout.Stat()
	if err != nil {
		return false
	}

	return info.Mode()&os.ModeCharDevice != 0
}
