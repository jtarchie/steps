# internal/testdocker

**Docker-backed tests run whenever a daemon is reachable**, and skip cleanly when one is not — they are no longer opt-in. They were, behind `STEPS_TEST_DOCKER=1`, and that is exactly how a containerized placed step shipped NOT WORKING: `task` was green for hours because the only test covering the feature never ran, and a later refactor broke it in silence. A test that guards a shipped feature does not get to be optional.

**`internal/testdocker` is the one helper**: `testdocker.Require(t)` skips without a daemon (probed once per process, not per test). A test that needs a tree in its container seeds a volume with `testdocker.Tree(t, dir)`, mounts it at `testdocker.TreePath`, and reads what the container wrote with `testdocker.ReadTree` — never a bind mount, which steps no longer makes (#246). `./e2e`'s `requireDockerE2E` is `Require` alone: TMPDIR stays where the OS put it, which proves a step never needs a path the daemon can see.

Still opt-in, because they are not docker-feature tests: the AWS/GCP fixtures' own `STEPS_TEST_AWS_*`/`STEPS_TEST_GCP_*`, `STEPS_TEST_CLAUDE` (a live Claude CLI, `e2e/e2e_cli_live_test.go`) and `STEPS_TEST_OPENSSH` (the system ssh client, `internal/venue/sshconfig_test.go`). Otherwise `go test ./...` needs no network and no credentials.
