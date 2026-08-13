package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/glamour"

	"github.com/jtarchie/steps/docs"
)

// DocsCmd renders the embedded documentation (docs/*.md — the same pages the
// web UI serves at /docs) in the terminal. No page name renders the index,
// which lists the rest.
type DocsCmd struct {
	Page string `arg:"" help:"doc page to read, e.g. resources (defaults to the index)" optional:""`
}

// Run renders the requested page through glamour when stdout is a terminal,
// and passes the raw markdown through when it is piped — so
// `steps docs agents | less` and scripting both stay useful.
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

	renderer, err := glamour.NewTermRenderer(glamour.WithAutoStyle(), glamour.WithWordWrap(100))
	if err != nil {
		return fmt.Errorf("steps docs: could not build renderer: %w", err)
	}

	rendered, err := renderer.Render(string(body))
	if err != nil {
		return fmt.Errorf("steps docs: could not render %s: %w", name, err)
	}

	fmt.Print(rendered)

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
