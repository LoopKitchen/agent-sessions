# loop-sessions

loop-sessions collects the AI coding sessions on every engineer's laptop into
one place your organisation runs. A small agent on each machine finds the
session stores that Claude Code and Codex write, captures new sessions as they
happen, redacts credentials, and uploads the result to a server you host. The
server keeps everything in Postgres and serves a dashboard: the session list,
full transcripts with tool calls and diffs, search, cost, per-skill usage, and
a fleet page that says which laptops are reporting and which are not. It
exists because a session's transcript is the only record of what an agent did,
and that record otherwise lives in one directory on one machine until it is
deleted.

## Why it is shaped this way

Three facts drove most of the design.

**A killed harness fires no end-of-session hook.** `SessionEnd` runs on a clean
exit and on `SIGTERM`, but not on `SIGKILL`, and those are disproportionately
the long, messy sessions worth capturing. So a daemon watches the harness
process directly and finalises the session when it disappears, by any means.
Recovery for a daemon that itself dies is handled at the next session start
rather than by a background timer. A daemon whose binary is replaced by an
upgrade restarts in place shortly after (same pid, new build, once the new file
has run its version verb), so a long session never keeps an old daemon.

**Transcripts contain live credentials.** Measured, not assumed: on one
machine, 30 files with Postgres passwords, 26 with JWTs, 18 Google API keys, 5
AWS keys, 3 PEM private keys. Scrubbing runs before any egress, and
over-redaction is treated as a defect too, since a transcript with every long
string blanked is worthless as a record.

**One logical task spans several files.** Continuations link backwards through
a marker that appears only on system records with no message body. Reading it
off content records, which is what the documentation implies, sets lineage on
essentially nothing, and a task spread across five files then counts as five
tasks, corrupting any rework metric built on top.

## Architecture

Two binaries, one Go module, no shared state except the server's database.

### Client: `loop-sessions`, on the laptop

The client is stdlib-only. It has to install on a machine with no module cache
and no network, so every avoidable dependency is one more way an install fails.

- **discover** (`internal/discovery`) probes the locations eight harnesses use
  on macOS, Linux and Windows, honours `CLAUDE_CONFIG_DIR` and `CODEX_HOME`,
  and reports three states per tool: found, installed with no sessions where
  it looked, absent. The middle state is surfaced as a question rather than
  reported as "not used".
- **capture** (`internal/capture`, `internal/hooks`) registers Claude Code
  lifecycle hooks in the harness's own settings file, tagged so they can be
  removed exactly. A hook translates one lifecycle event into canonical
  events, scrubs them, writes them to the spool and returns. Nothing on the
  hook path touches the network, because a hook runs inline with a person's
  turn.
- **scrub** (`internal/scrub`) redacts credentials before an event is written
  to disk. Every hit becomes `[REDACTED:<kind>]` so a leaked Stripe key and a
  leaked JWT stay distinguishable downstream. See Security below.
- **backfill** (`internal/backfill`) walks the transcripts already on the
  machine (Claude Code, including subagent and workflow files; Codex rollouts)
  and emits the same events with the original timestamps, so an imported
  session differs from a live one only in when it arrived.
- **spool** (`internal/spool`) is the durable outbox: immutable payload files
  published by atomic rename under `pending/`, with `quarantine/` for what the
  server explicitly refused and `parked/` for what it neither accepted nor
  refused eight times running.
- **drain** (`internal/drain`) is the only component that talks to the
  network. It acknowledges exactly what the server said it stored, retries
  transport failures forever with full-jitter backoff, and treats only an
  explicit rejection as final.
- **daemon** (`internal/daemon`) is spawned at session start, watches the
  harness pid, flushes on a cadence, finalises the session when the process
  goes away, and reconciles sessions an earlier daemon left open. It also runs
  the self-upgrade check (`internal/upgrade`) and the daily repair walk.
- **health** (`internal/health`) builds the self-telemetry the fleet page is
  drawn from: conditions rather than counters, on a fixed cadence whether or
  not anything happened, with no session ids, paths or prompt text in it.

### Server: `loop-sessions-server`

- **ingest** (`server/ingest`) receives `POST /v1/events` and
  `POST /v1/health` from enrolled devices. Every item is answered
  individually, redelivery is a no-op through the session rollup and the cost
  arithmetic, and every payload is scrubbed a second time on arrival in case
  the agent's rule set has fallen behind.
- **store** (`server/store`) is the Postgres layer. Twenty-two migrations
  run at boot. Events are the durable record; sessions, turns, messages,
  links and artifacts are derived from them by a resumable, versioned runner
  (`server/store/derive`) and can be rebuilt at any time. Authorization is a
  predicate inside the read, and a read of a colleague's session writes its
  own audit row in the same transaction.
- **auth** (`server/auth`) verifies Firebase ID tokens for people (Google
  provider only, domain-checked against `ALLOWED_DOMAINS`, then looked up in
  the principals roster) and long-lived device tokens for agents. The server
  mints its own HMAC session cookie and holds no OAuth credential.
- **web** (`server/web`) is the dashboard: server-rendered `html/template`,
  no build step, `script-src 'none'` on every page except the few that opt in
  to one first-party script.
- **api** (`server/api`) is the JSON read API behind the same cookie:
  sessions, transcripts, search, share links, skill usage, and the resume
  bundle that lets someone continue a colleague's session on their own
  machine.
- **admin** (`server/admin`, `server/fleet`) is the operator surface:
  the principals roster, the fleet page, and the access log. The system
  refuses to end up with zero admins.
- **skillusage** (`server/skillusage`) accepts skill invocations from
  emitters that are not an enrolled laptop.
- **Optional integrations.** `RELEASE_BUCKET` mounts `/install.sh` and
  `/dl/<channel>/*`, proxied from a private Cloud Storage bucket, so the
  server can be the release host for its own agents. `SLACK_BOT_TOKEN`
  enables the Slack mirror (`server/slack`), one message per finished
  session, off for everyone until a person turns it on. The `export`
  subcommand (`server/export`) copies the derived tables to Cloud Storage as
  gzip JSONL and loads them into BigQuery, one run per invocation, for
  analysts who should not be reading the primary. None of the three is
  needed to run the server.

## Layout

| Path | What it is |
|---|---|
| `cmd/loop-sessions/` | the client CLI and agent: install, uninstall, status, discover, doctor, pause, resume, backfill, mirror, hook, daemon, version |
| `internal/backfill/` | historical transcripts (Claude Code, Codex) to events, timestamps intact |
| `internal/capture/` | live Claude Code hooks to spooled events |
| `internal/config/` | the client's on-disk settings and the record of what a person consented to |
| `internal/daemon/` | session lifetime, crash detection, reconciliation |
| `internal/discovery/` | locate session stores on any machine |
| `internal/drain/` | delivery to the server with backoff and per-item acknowledgement |
| `internal/enroll/` | browser sign-in exchanged for a device credential over a loopback listener |
| `internal/event/` | the canonical, tool-agnostic record everything speaks; `CaptureSchema` lives here |
| `internal/health/` | the self-telemetry report the fleet page is built on |
| `internal/hooks/` | register and remove this agent's hooks in the harness's settings file |
| `internal/links/` | find and classify the URLs a session mentioned (PRs, issues, runbooks) |
| `internal/normalize/` | the one classifier for which "user" records a person actually typed |
| `internal/pipeline/` | the adapters that wire capture to the spool and the spool to delivery |
| `internal/receiver/` | reference server for the drain's contract; the client's end-to-end test fixture |
| `internal/scrub/` | credential redaction |
| `internal/skilllog/` | the skill-usage log strings two server packages share |
| `internal/spool/` | the durable outbox |
| `internal/upgrade/` | replace the agent binary when the published digest differs |
| `server/cmd/loop-sessions-server/` | the server entrypoint; `export` subcommand |
| `server/app/` | configuration from the environment, wiring, route table, drain policy |
| `server/api/` | JSON read API: sessions, transcripts, search, shares, skills, resume |
| `server/admin/` | principals roster, fleet page, access log |
| `server/auth/` | Firebase ID token verification, device tokens, session cookies, the permission rule |
| `server/export/` | Cloud Storage and BigQuery export |
| `server/fleet/` | fleet evaluation: who is reporting, who is stale, what to do |
| `server/ingest/` | the upload endpoints |
| `server/skillusage/` | skill invocations from non-laptop emitters |
| `server/slack/` | the Slack mirror |
| `server/store/` | Postgres, 22 migrations, retention, the derive runner |
| `server/store/derive/` | the pure fold from stored events to turns and threads |
| `server/web/` | the dashboard: templates, static assets, transcript rendering |
| `install/` | `install.sh`, `uninstall.sh`, and the page that explains them |
| `examples/deploy-gcp/` | a complete Cloud Run + Cloud SQL deployment, with placeholders |
| `docs/UPGRADES.md` | when a change reaches sessions already captured, and how |
| `docs/RELEASE.md` | the release layout and every consumer of it |

## Self-hosting

### Prerequisites

- **Postgres 15 or later.** `server/store/migrations/0001_init.sql` uses
  `NULLS NOT DISTINCT`, which does not exist before 15.
- **A Firebase project with Google enabled as a sign-in provider.** The
  server verifies Firebase ID tokens against Google's published certificates
  and calls no other Google API for sign-in. You need the project id and its
  web API key, and the host in
  `PUBLIC_URL` must be in the project's authorized-domains list or the
  sign-in popup fails in the browser with nothing in the server log.
- **Go 1.26** to build from source, or Docker to build the server image.

### Environment

The server reads its whole configuration from the environment
(`server/app/config.go`). Every missing required variable is reported in one
error at boot.

Required:

| Variable | Meaning |
|---|---|
| `DATABASE_HOST` | hostname for TCP, or a directory starting with `/` for a unix socket |
| `DATABASE_NAME`, `DATABASE_USER`, `DATABASE_PASSWORD` | the Postgres role and database |
| `SESSION_KEY` | base64, decoding to at least 32 bytes; signs the dashboard cookie |
| `FIREBASE_PROJECT_ID` | the Firebase project both sign-in surfaces verify against |
| `FIREBASE_API_KEY` | the project's web API key; public by design, and logged verbatim |
| `ALLOWED_DOMAINS` | comma-separated email domains permitted to sign in |
| `PUBLIC_URL` | the origin people reach the dashboard at; `http://` switches cookies to insecure for local use |

`ALLOWED_DOMAINS` is a coarse gate, not the authorization control: a Firebase
token carries no proof of Workspace membership, so any Google account on a
listed domain can reach the sign-in page. The principals roster decides what
each person may do, and `ADMIN_EMAILS` is how the first admin gets there
without touching SQL.

Optional:

| Variable | Default | Meaning |
|---|---|---|
| `PORT` | `8080` | listener port |
| `DATABASE_PORT` | `5432` | sent for a unix socket too, because the socket file is named after it |
| `FIREBASE_AUTH_DOMAIN` | `<project>.firebaseapp.com` | the auth domain the sign-in page loads |
| `ADMIN_EMAILS` | unset | comma-separated addresses, each in `ALLOWED_DOMAINS`, given an admin row at boot when they have none; an existing row is never changed, so it grants a first admin but never re-grants, promotes or re-enables |
| `DOMAIN_ALIASES` | unset | comma-separated `a.example:b.example` pairs of `ALLOWED_DOMAINS` entries that one Workspace serves as the same accounts, so the same local part on either is one person; leave unset with one domain |
| `RELEASE_BUCKET` | unset | Cloud Storage bucket to serve `/install.sh` and `/dl/` from; unset means those routes are not mounted |
| `SLACK_BOT_TOKEN` | unset | enables the Slack mirror |
| `LOOP_SESSIONS_SLACK_SIGNING_SECRET` | unset | enables Slack interactive replies |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `RETENTION_BODY_DAYS`, `RETENTION_SESSION_DAYS` | `0` | days before event bodies, then whole sessions, are deleted; `0` keeps forever and is warned about at boot |
| `DERIVE_WINDOW`, `DERIVE_ROWS_PER_SEC`, `DERIVE_ROWS_PER_BATCH` | `02-06`, `1500`, `5000` | bounds on the background derive runner; the window is `HH-HH` in the server's `TZ` (or `always`) and gates only the body-reading steps |

### Local development

Start Postgres:

```sh
docker run -d --name loop-sessions-pg \
  -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=loop_sessions \
  -p 5432:5432 postgres:16
```

Set the environment and run the server. Use `127.0.0.1`, not `localhost`: the
client accepts plain `http://` only to the loopback literal, and the
enrollment page posts the sign-in token only to `127.0.0.1`.

```sh
export DATABASE_HOST=127.0.0.1
export DATABASE_NAME=loop_sessions
export DATABASE_USER=postgres
export DATABASE_PASSWORD=postgres
export SESSION_KEY="$(openssl rand -base64 32)"
export FIREBASE_PROJECT_ID=your-firebase-project
export FIREBASE_API_KEY=your-firebase-web-api-key
export ALLOWED_DOMAINS=example.com
export ADMIN_EMAILS=you@example.com
export PUBLIC_URL=http://127.0.0.1:8080

go run ./server/cmd/loop-sessions-server
```

Migrations run at boot. Open `http://127.0.0.1:8080` and sign in with a Google
account on an allowed domain.

Build and enrol the client against it:

```sh
make build
./loop-sessions install --endpoint http://127.0.0.1:8080
```

`install` shows what it found on the machine, opens the browser to sign in,
registers the capture hooks, and imports the history already on disk
(`--backfill-since 30d` to limit it, `--skip-backfill` to skip it,
`--skip-hooks`, `--skip-signin`). The client keeps its state under
`~/.loop/sessions`, or `LOOP_SESSIONS_HOME`.

Tests:

```sh
make lint        # gofmt, go vet, shellcheck
make test        # go test -race ./...
```

The `*_integration_test.go` files need a database and the `integration` build
tag; they skip themselves otherwise:

```sh
docker run -d -e POSTGRES_PASSWORD=test -e POSTGRES_DB=loop_sessions_test -p 55433:5432 postgres:16
LOOP_SESSIONS_TEST_DSN=postgres://postgres:test@127.0.0.1:55433/loop_sessions_test \
  go test -tags integration ./server/...
```

Each run works inside its own schema and drops it afterwards.

### Releases

The release layout and every consumer of it are in [docs/RELEASE.md](docs/RELEASE.md).

`make release` cross-compiles the client for `darwin/arm64`, `darwin/amd64`,
`linux/amd64` and `linux/arm64` into `dist/` and writes two files beside them:

- `dist/SHA256SUMS`, in `sha256sum` format, covering every binary;
- `dist/latest.json`, naming the build (`version`, full `commit`,
  `build_date`, `capture_schema`) and the digest of every asset, generated
  from `SHA256SUMS` so the two cannot disagree.

The target refuses a dirty tree and a binary whose stamped `vcs.revision` is
not the commit it will name (`ALLOW_DIRTY=1` skips both for a scratch build).
`make verify` re-checks `dist/` against its own manifest. The default endpoint
stamped into the binary comes from `ENDPOINT` (`make release
ENDPOINT=https://sessions.example.com`); a person can always override it with
`install --endpoint`.

The layout is a contract between the Makefile, `install/install.sh`,
`internal/upgrade` and `server/app/download.go`. Upload the contents of
`dist/` to `<base>/<channel>/` on any HTTPS host, where `<channel>` is
`latest` for the fleet or `canary` for the machines that take a build first.
The installer then needs exactly three URLs:

```
<base>/<channel>/SHA256SUMS
<base>/<channel>/latest.json          optional; older channels have none
<base>/<channel>/loop-sessions_<os>_<arch>
```

```sh
LOOP_SESSIONS_BASE_URL=https://releases.example.com sh install/install.sh
```

`install.sh` is POSIX sh, fetches HTTPS-only (plain HTTP to loopback only, for
testing against a local host), verifies the SHA-256 before the file is ever
made executable, deletes it on a mismatch, installs to `~/.local/bin` without
`sudo`, and hands off to `loop-sessions install` for sign-in. `install/README.md`
explains every step for someone deciding whether to trust it.

Self-upgrade works from the same layout, read from the server the machine is
enrolled with: the daemon fetches `<endpoint>/dl/<channel>/SHA256SUMS`,
compares the published digest with its own bytes, and replaces itself only
when they differ. Staleness is a checksum, not a version number, so a
deliberate rollback propagates too. The server answers `/dl/` only when
`RELEASE_BUCKET` is set; without it, a machine logs that the check failed and
keeps its binary, and `daemon --upgrade-now` or a re-run of the installer
moves it. A development build (`version` prints `dev`) never upgrades, and
`LOOP_SESSIONS_NO_UPGRADE=1` disables the check.

### Deploying on Google Cloud

`examples/deploy-gcp/` is a complete, opinionated deployment: a Cloud Run
service in front of Cloud SQL Postgres, the two-stage distroless Dockerfile,
a Cloud Build recipe, a CI setup script for Workload Identity, a release
workflow that publishes `dist/` to a bucket, log-based metrics and alert
policies, and the BigQuery export job. Every project-specific value is a
placeholder. It is an example, not the only way to run this; the server needs
Postgres and a Firebase project and nothing else.

## Using the client

```
loop-sessions install             set up: find sessions, sign in, start capturing
loop-sessions uninstall           stop capturing and clean up (--purge also deletes captured data waiting to upload)
loop-sessions status              what is being captured and whether it is working (--json)
loop-sessions discover            re-scan this machine for agent session files (--json)
loop-sessions backfill            import the sessions already on this machine
                                  (--since all|30d|2026-07-01, --session <id>, --dry-run)
loop-sessions pause [--for 2h]    stop capturing until resumed
loop-sessions resume              start capturing again
loop-sessions doctor              diagnose problems and say how to fix them
                                  (--redrive, --replay-quarantine)
loop-sessions mirror              list, use, off, attach, status: the Slack mirror from the terminal
loop-sessions version             print the agent version

loop-sessions install --hooks-only   put the capture hooks back without signing in again
loop-sessions daemon --upgrade-now   check the release host now and replace this binary
```

`hook` and `daemon` are invoked by Claude Code and the installer, not by you.
The hook path never returns non-zero and never writes to stdout, because a
hook that errors or chatters degrades the editor for the person using it.

## Security

Report vulnerabilities as described in [SECURITY.md](SECURITY.md).

The scrubber (`internal/scrub`) runs on the laptop before an event is written
to the spool, which is before anything leaves the machine. It covers twenty
credential classes (`Kinds` in `internal/scrub/scrub.go`): SSH and PEM private
keys, database passwords in connection strings, Slack webhooks and tokens,
Anthropic, OpenAI, Stripe, GitHub, AWS and Google keys and tokens, JWTs,
`Authorization` headers, this product's own device tokens, and a generic rule
gated on an adjacent secret-looking key name plus an entropy floor. The server
runs the same rules again on ingest and counts what the agent missed
separately, so a machine whose agent has fallen behind is visible. The
redaction count travels with each event. The test fixtures in `internal/scrub`
are synthetic values that match real key shapes; none was ever issued.

Other properties worth knowing before you run it:

- A session the viewer may not see and a session that does not exist produce
  byte-identical responses, so the dashboard cannot be used to learn that a
  colleague ran something.
- Every read of someone else's transcript writes an audit row in the same
  transaction, visible on the admin access log.
- Event archive and per-event pages ship no JavaScript and carry
  `script-src 'none'`; the filter bars and the opt-in continuous view load one
  first-party script each, and the filter bars work without it.
- The agent holds one credential, a device token minted at enrollment; the
  Firebase token is never persisted on the laptop.
- The health report contains no session ids, project paths or prompt text.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). The suite is 1,779 test functions
across 33 packages and runs with the race detector in CI.

## License

MIT. See [LICENSE](LICENSE).
