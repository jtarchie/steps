// Package main is the steps entrypoint. It holds nothing but the process
// boundary — hand the command line to internal/cli, turn the error it
// returns into an exit code — so that every command, and the end-to-end
// suite that drives them, lives in a package something can import.
package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/jtarchie/steps/internal/cli"
	"github.com/jtarchie/steps/internal/outcome"
)

func main() {
	cli.InitLogging(cli.DefaultLogLevel)

	err := cli.Run(os.Args[1:])
	if err != nil {
		code := outcome.ExitCode(err)

		slog.Debug("main.run", "error", err, "code", code)
		fmt.Fprintf(os.Stderr, "steps: error: %v\n", err)
		os.Exit(code)
	}

	slog.Debug("main.exit", "code", outcome.ExitOK)
}
