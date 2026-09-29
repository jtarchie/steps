// Package main is the shim alone: the program a venue pushes to a worker,
// without the orchestrator, the engine client or the web UI that make the
// steps binary a hundred megabytes. It answers the same argv as `steps _shim`.
package main

import (
	"fmt"
	"os"

	"github.com/jtarchie/steps/internal/shim"
)

func main() {
	err := shim.Main(os.Args[1:], os.Stdin, os.Stdout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "steps-shim: error: %v\n", err)
		os.Exit(1)
	}
}
