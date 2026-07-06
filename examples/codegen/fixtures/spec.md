# greet — a tiny greeting CLI

Write a small command-line tool called `greet`.

- With no arguments it prints `Hello, world!`.
- `--name NAME` greets NAME instead of `world` (e.g. `greet --name Ada` prints
  `Hello, Ada!`).
- `--shout` uppercases the entire greeting (`HELLO, WORLD!`).
- The two flags compose, in any order.
- An unknown flag is an error: print usage to stderr and exit non-zero.

Deliver exactly two files:

- `greet.sh` — the CLI itself.
- `greet_test.sh` — an executable test script that runs `greet.sh` (invoke it
  with `bash greet.sh ...`, same directory) and exits non-zero if any behaviour
  above is wrong. This is the acceptance gate, run as `bash greet_test.sh`.

Do not add any other files.
