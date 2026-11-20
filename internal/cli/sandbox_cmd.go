package cli

import "github.com/jtarchie/steps/internal/shell"

// SandboxCmd is the helper a linux host fence re-executes steps as (shell.SandboxHelper): it restricts itself with Landlock, then becomes the step's command. Hidden, because nobody types it.
type SandboxCmd struct {
	Write []string `help:"a directory the command may write" name:"write"                     sep:"none"`
	Hide  []string `help:"a path the command may not read"   name:"hide"                      sep:"none"`
	Argv  []string `arg:""                                   help:"the command to run fenced" passthrough:""`
}

// Run returns only when the fence or the exec failed; otherwise this process is the command now.
func (s *SandboxCmd) Run() error {
	argv := s.Argv
	if len(argv) > 0 && argv[0] == "--" {
		argv = argv[1:] // kong's passthrough keeps the separator
	}

	return shell.SandboxExec(s.Write, s.Hide, argv) //nolint:wrapcheck // shell says it is the sandbox
}
