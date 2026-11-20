package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jtarchie/steps/internal/config"
)

// FmtCmd rewrites pipeline files into the one standard layout (docs/fmt.md).
type FmtCmd struct {
	Paths []string `arg:""                                                             help:"pipeline files or directories to format; - reads standard input and prints the result"`
	Check bool     `help:"change nothing; fail naming each file that is not formatted"`
	Diff  bool     `help:"change nothing; show what would change"`
}

// Run formats each path, carrying on past a file it refuses so one invocation names every problem.
func (f *FmtCmd) Run(streams Streams) error {
	var errs []error

	unformatted := 0

	for _, path := range f.Paths {
		if path == "-" {
			errs = append(errs, f.stdin(streams))

			continue
		}

		info, err := os.Stat(path)
		if err != nil {
			errs = append(errs, fmt.Errorf("steps fmt: %w", err))

			continue
		}

		if !info.IsDir() {
			changed, err := f.file(streams, path, info.Mode())
			errs = append(errs, err)

			if changed {
				unformatted++
			}

			continue
		}

		n, err := f.dir(streams, path)
		errs = append(errs, err)
		unformatted += n
	}

	if f.Check && unformatted > 0 {
		errs = append(errs, fmt.Errorf("steps fmt: %d file(s) not formatted", unformatted))
	}

	return errors.Join(errs...)
}

// dir formats every .yml/.yaml under root, naming each one skipped for not being a pipeline.
func (f *FmtCmd) dir(streams Streams, root string) (int, error) {
	var errs []error

	unformatted := 0

	walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		ext := filepath.Ext(path)
		if entry.IsDir() || (ext != ".yml" && ext != ".yaml") {
			return nil
		}

		info, err := entry.Info()
		if err != nil {
			return err //nolint:wrapcheck // WalkDir's caller wraps
		}

		changed, err := f.file(streams, path, info.Mode())
		if errors.Is(err, config.ErrNotPipeline) {
			sayf(streams.Err, "skipped %s: %v\n", path, config.ErrNotPipeline)

			return nil
		}

		errs = append(errs, err)

		if changed {
			unformatted++
		}

		return nil
	})
	if walkErr != nil {
		errs = append(errs, fmt.Errorf("steps fmt: %w", walkErr))
	}

	return unformatted, errors.Join(errs...)
}

// file formats one pipeline in place (or checks or diffs it), reporting whether it was not already formatted.
func (f *FmtCmd) file(streams Streams, path string, mode fs.FileMode) (bool, error) {
	source, err := os.ReadFile(path) //nolint:gosec // the file the user asked to format
	if err != nil {
		return false, fmt.Errorf("steps fmt: %w", err)
	}

	formatted, err := config.Format(source, config.DirFS(filepath.Dir(path)))
	if err != nil {
		return false, fmt.Errorf("steps fmt: %s: %w", path, err)
	}

	if bytes.Equal(source, formatted) {
		return false, nil
	}

	switch {
	case f.Diff:
		return true, showDiff(streams.Out, path, source, formatted)
	case f.Check:
		say(streams.Out, path)

		return true, nil
	}

	err = os.WriteFile(path, formatted, mode.Perm()) //nolint:gosec // the file the user asked to format, rewritten in place
	if err != nil {
		return true, fmt.Errorf("steps fmt: %w", err)
	}

	say(streams.Out, path)

	return true, nil
}

// stdin is the editor-integration mode: the formatted pipeline on stdout, includes resolved from the working directory.
func (f *FmtCmd) stdin(streams Streams) error {
	source, err := io.ReadAll(streams.In)
	if err != nil {
		return fmt.Errorf("steps fmt: %w", err)
	}

	formatted, err := config.Format(source, config.DirFS("."))
	if err != nil {
		return fmt.Errorf("steps fmt: <stdin>: %w", err)
	}

	switch {
	case f.Diff:
		return showDiff(streams.Out, "<stdin>", source, formatted)
	case f.Check && !bytes.Equal(source, formatted):
		return errors.New("steps fmt: <stdin> is not formatted")
	case f.Check:
		return nil
	}

	_, err = streams.Out.Write(formatted)
	if err != nil {
		return fmt.Errorf("steps fmt: %w", err)
	}

	return nil
}

// showDiff hands both versions to the system diff, as gofmt -d once did. ponytail: a host with no diff on PATH gets an error; an in-process unified diff is the upgrade if that host matters.
func showDiff(out io.Writer, label string, before, after []byte) error {
	dir, err := os.MkdirTemp("", "steps-fmt-")
	if err != nil {
		return fmt.Errorf("steps fmt: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	oldPath, newPath := filepath.Join(dir, "old"), filepath.Join(dir, "new")

	err = errors.Join(os.WriteFile(oldPath, before, 0o600), os.WriteFile(newPath, after, 0o600)) //nolint:gosec // paths inside our own temp dir
	if err != nil {
		return fmt.Errorf("steps fmt: %w", err)
	}

	var diff bytes.Buffer

	cmd := exec.Command("diff", "-u", "-L", label, "-L", label+" (formatted)", oldPath, newPath) //nolint:gosec,noctx // fixed argv over our own temp files
	cmd.Stdout = &diff

	err = cmd.Run()

	var exit *exec.ExitError
	if err != nil && (!errors.As(err, &exit) || exit.ExitCode() != 1) {
		return fmt.Errorf("steps fmt: diff: %w", err)
	}

	_, err = io.WriteString(out, strings.TrimSuffix(diff.String(), "\n")+"\n")
	if err != nil {
		return fmt.Errorf("steps fmt: %w", err)
	}

	return nil
}
