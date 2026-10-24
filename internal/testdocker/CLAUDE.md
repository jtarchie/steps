# internal/testdocker

**Docker-backed tests run whenever a daemon is reachable**, and skip cleanly when one is not — they are no longer opt-in. They were, behind `STEPS_TEST_DOCKER=1`, and that is exactly how a containerized placed step shipped NOT WORKING: `task` was green for hours because the only test covering the feature never ran, and a later refactor broke it in silence. A test that guards a shipped feature does not get to be optional.

**`internal/testdocker` is the one helper**: `testdocker.Require(t)` skips without a daemon (probed once per process, not per test), and a test that BIND-MOUNTS also takes `testdocker.MountableDir(t)`, because on macOS the daemon runs in a VM that does not share `/var/folders` and answers an unshared mount with an EMPTY directory — a step that then succeeds and produces nothing. `./e2e`'s `requireDockerE2E` is both, with TMPDIR moved there (which forbids `t.Parallel`; a docker+ test, whose trees live in volumes, calls `Require` alone).

Still opt-in, because they are not docker-feature tests: the AWS/GCP fixtures' own `STEPS_TEST_AWS_*`/`STEPS_TEST_GCP_*`, `STEPS_TEST_CLAUDE` (a live Claude CLI, `e2e/e2e_cli_live_test.go`) and `STEPS_TEST_OPENSSH` (the system ssh client, `internal/venue/sshconfig_test.go`). Otherwise `go test ./...` needs no network and no credentials.
