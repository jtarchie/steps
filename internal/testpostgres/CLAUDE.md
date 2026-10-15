# internal/testpostgres

**A real Postgres for tests, with no docker daemon.** `testpostgres.Start(settings)` runs Postgres 17 from `embedded-postgres`' binaries on a free loopback port with a random superuser password, and `Stop` removes it. It replaced a `postgres:17-alpine` container started through `dockerapi`, which made every Postgres test skip on a machine with docker down — the conformance suite among them.

- **The first run on a machine downloads the binaries** (to `~/.embedded-postgres-go`, from Maven Central); every later run uses the cache. A fetch that fails returns `ErrUnavailable` and the caller skips, the way a missing docker daemon did. Any other `Start` error is a failure.
- **`Fast` is for tests, `nil` is for measurements.** `Fast` turns fsync off and widens `max_connections`; `task loadtest` with `STEPS_LOADTEST_DB=postgres` passes none, because durability off would flatter the numbers.
- **One server per test process**, in `os.TempDir()/steps-testpg/<pid>/`. `Start` sweeps directories whose pid is gone and signals their postmaster, but only if `ps` still calls that pid a postgres — a run killed outright never reached its own `Stop`.
- **Test-only, enforced by depguard** (`TestSSHDTestOnly`'s deny list), like `testdocker`.
