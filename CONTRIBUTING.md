# Contributing

Thanks for looking. This page says what a change needs before it can merge,
and why.

## Before you start

Read the README's Architecture section and the package doc comment of
whatever you are touching. The doc comments say why a package is shaped the
way it is, and most of the rules below exist because a plausible change
broke one of those reasons.

Small fixes can go straight to a pull request. For anything that changes
behaviour, what an event carries, or the release layout, open an issue first
so the design can be discussed before the code.

## Building and testing

```sh
make lint && go test -race ./...
```

That is what CI runs (`.github/workflows/ci.yml`), plus a cross-compile of the
client and a build of the server image. `make lint` is `gofmt -l`, `go vet`,
and `shellcheck` over the shell scripts; `make test` is the same
`go test -race ./...`.

Integration tests (`*_integration_test.go`, build tag `integration`) need a
Postgres 15+ database and skip themselves without one:

```sh
LOOP_SESSIONS_TEST_DSN=postgres://postgres:test@127.0.0.1:55433/loop_sessions_test \
  go test -tags integration ./server/...
```

New behaviour comes with a test. A change to `internal/scrub` comes with a
fixture that shows the case, and the fixture must be synthetic: a value that
matches a real key's shape but was never issued.

## Two constants to think about

- `event.CaptureSchema` (`internal/event/event.go`): bump it when the agent
  will extract something better or different from the same transcript.
- `store.DerivedSchema` (`server/store`): bump it when the server will derive
  something better or different from events already stored.

`docs/UPGRADES.md` explains both, what a bump costs, and when not to bump.
Say in the commit message which one you considered and why you left it
alone if you did.

## Commits

Use conventional commit subjects: `feat(scope): ...`, `fix(scope): ...`,
`docs: ...`, `refactor(scope): ...`, `test(scope): ...`, `chore: ...`. The
scope is a package or directory (`drain`, `server/ingest`, `install`).

The body explains why. The diff already says what.

## Pull requests

- One change per pull request. A refactor and the behaviour change it enables
  are two pull requests.
- The description says what problem the change solves and how you verified
  it. Paste the relevant test output when the change is not obviously
  covered by CI.
- CI must be green. A failing cross-compile or a Docker build that no longer
  builds is a failing PR.
- Keep the client dependency-free. `cmd/loop-sessions` and `internal/` use the
  standard library only; a new `require` in `go.mod` for the client is a
  design discussion, not a routine change.
- Changes to the release layout (`Makefile` `release` target,
  `install/install.sh`, `internal/upgrade`, `server/app/download.go`) must
  update all four together. They are one contract.

## No company-specific identifiers or customer data

This repository is the public cut of an internal tool. Nothing in it may name
a specific organisation's infrastructure or people:

- no cloud project ids, project numbers, service-account addresses, bucket
  names, or production hostnames;
- no employee names, personal email addresses, or chat workspace, channel or
  user ids;
- no customer names and no transcript content, real or "lightly edited";
- test fixtures use `@example.com` addresses and `example.com` domains.

CI enforces the mechanical part with the identifier gate workflow
(`.github/workflows/identifier-gate.yml`), which runs
`.github/scripts/identifier-gate.sh` over the tree; a hit fails the build.
The script greps for the shape of internal identifiers (chat ids, home
directory paths, cloud hostnames and project ids, mail addresses outside the
example domains). In this repository's own CI it also runs an extended,
non-public pattern supplied through the `IDENTIFIER_GATE_PATTERNS` repository
secret; pull requests from forks run the generic pattern only, and a
maintainer re-runs the full gate before merging. Run the script locally
before opening a pull request:

```
bash .github/scripts/identifier-gate.sh
```

The gate cannot recognise every identifier, so the reviewer checks the rest.
When you need an example value, use the placeholders the example deployment
already uses (`__PROJECT__`, `sessions.example.com`, `you@example.com`).

## Conduct

This project follows the [Contributor Covenant](CODE_OF_CONDUCT.md).

## Security

Do not report vulnerabilities in a pull request or issue. See
[SECURITY.md](SECURITY.md).
