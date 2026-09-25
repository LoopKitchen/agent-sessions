# Releases: what is published, where it can live, and who reads it

A loop-sessions release is a directory of static files. Nothing about it
needs a particular host: the installer, the agent's self-upgrade and the
server all fetch the same handful of names from `<base>/<channel>/`, and
anything that answers HTTPS for those names is a release host. This page
describes the layout, how `make release` produces it, and exactly how each
consumer reads it, so that hosting it somewhere new is a matter of copying
files rather than reading Go.

The names are a contract between four places: the `Makefile` release
target, `install/install.sh`, `internal/upgrade` and `server/app/download.go`.
`internal/upgrade/upgrade_test.go`
(`TestTheAssetNameMatchesWhatTheReleaseTargetPublishes`) reads the Makefile
and the installer and fails when the binary name drifts between them, which
is the one drift that breaks upgrades silently while installs keep working.

## The files

`make release` writes everything into `dist/`:

| File | What it is |
|---|---|
| `loop-sessions_darwin_arm64` | the agent binary, one per platform |
| `loop-sessions_darwin_amd64` | |
| `loop-sessions_linux_amd64` | |
| `loop-sessions_linux_arm64` | |
| `SHA256SUMS` | `sha256sum` output for the four binaries, one `<hex>  <name>` per line |
| `latest.json` | the build's identity and the digest of every asset |

The platform list is `PLATFORMS` in the Makefile: `darwin/arm64 darwin/amd64
linux/amd64 linux/arm64`. Binaries are named `loop-sessions_<GOOS>_<GOARCH>`
with no extension and no version in the name; the version lives inside the
binary (`loop-sessions version` prints it) and in `latest.json`.

`SHA256SUMS` is written from inside `dist/` (`cd $(DIST) && sha256sum
loop-sessions_* > SHA256SUMS`), so the names it carries have no directory
part. Both readers tolerate the `*` binary-mode marker before a name and
compare by base name, but the file as published never has either.

`latest.json` is generated from `SHA256SUMS` rather than from the build loop,
so the two cannot disagree about a digest:

```json
{
  "version": "<VERSION>",
  "commit": "<full 40-character sha>",
  "build_date": "<RFC 3339, UTC>",
  "capture_schema": 4,
  "assets": {
    "loop-sessions_darwin_amd64": "<sha256 hex>",
    "loop-sessions_darwin_arm64": "<sha256 hex>",
    "loop-sessions_linux_amd64": "<sha256 hex>",
    "loop-sessions_linux_arm64": "<sha256 hex>"
  }
}
```

`commit` is the full sha on purpose: the server's fleet page compares it with
the VCS stamp each machine reports, and a 7-character prefix is enough for a
person and not for a machine. `capture_schema` is read from the constant
`CaptureSchema` in `internal/event/event.go` at build time, so the manifest
cannot claim an extraction version the code does not have.

## Building a release

```sh
make release ENDPOINT=https://sessions.example.com
make verify
```

What `make release` does, in order (`Makefile`, target `release`):

1. `make clean`, then refuses to run when `git status --porcelain` is not
   empty. `ALLOW_DIRTY=1` skips this and the stamp check below, for scratch
   builds only.
2. Reads `CAPTURE_SCHEMA` from `internal/event/event.go` and stops if it
   cannot.
3. Cross-compiles the four platforms with `CGO_ENABLED=0 go build -trimpath
   -ldflags '-s -w -X main.Version=$(VERSION) -X main.BuildDate=$(BUILD_DATE)
   -X main.defaultEndpoint=$(ENDPOINT)'`. Static, path-stripped and
   symbol-stripped, so two people building the same commit get the same
   bytes and no home directory leaks into the binary.
4. Runs `go version -m` on the linux/amd64 binary and refuses to write a
   manifest when its `vcs.revision` is not `COMMIT` or it carries
   `vcs.modified=true`. A build from a linked git worktree is stamped with
   the primary checkout's HEAD, which is the case this catches: the fleet
   page could never match such a binary to `latest.json`.
5. Writes `SHA256SUMS`, then `latest.json` (`make manifest`), then prints
   both and the stamp the fleet will see.

The variables that shape a build:

| Variable | Default | Meaning |
|---|---|---|
| `VERSION` | `git describe --tags --always --dirty` | stamped into the binary and `latest.json` |
| `COMMIT` | `git rev-parse HEAD` | the full sha in `latest.json` |
| `BUILD_DATE` | `date -u +%Y-%m-%dT%H:%M:%SZ`, read once and exported | stamped into the binary and `latest.json`; the `BuildDate` the health report carries when the toolchain has no `vcs.time` (a `git archive` build) |
| `ENDPOINT` | see the Makefile | the server URL stamped into the agent as `defaultEndpoint`; `loop-sessions install --endpoint` overrides it per machine |
| `ALLOW_DIRTY` | unset | skip the clean-tree and stamp checks |

`ENDPOINT` is the one that makes a release yours. The agent talks to exactly
one server, and a release built for one deployment points at it by default;
build with `ENDPOINT=https://<your server>` or tell every machine to pass
`--endpoint` at install.

`make verify` re-checks `dist/` against its own output: `sha256sum -c
SHA256SUMS` (or `shasum -a 256 -c`), then, when `python3` is on the PATH, an
assertion that `latest.json` carries `version`, `commit` and a non-empty
`assets` map.

`make release` does not copy the installer into `dist/`. If the published
one-liner should work, publish `install/install.sh` beside the channel's files
as well (see below). `install/uninstall.sh` may be published at the base too,
for people without the binary; the installer itself points at
`loop-sessions uninstall`, which needs no download.

## Hosting: the layout

```
<base>/
  latest/
    SHA256SUMS
    latest.json
    loop-sessions_darwin_arm64
    loop-sessions_darwin_amd64
    loop-sessions_linux_amd64
    loop-sessions_linux_arm64
    install.sh            # optional; what the server's /install.sh serves
  canary/
    (the same seven names)
  uninstall.sh            # optional; for people who no longer have the binary
```

`<base>` is any HTTPS URL. A directory on a static web host, an object
bucket behind a CDN, or the loop-sessions server's own `/dl/` proxy all
satisfy it; the installer requires only that `GET <base>/<channel>/<name>`
answers with the bytes. Plain `http://` is accepted only for `127.0.0.1`,
`localhost` and `[::1]`, which exists so the installer can be tested against
a local release host (`install/install.sh`, `is_loopback`).

Two channels exist, `latest` and `canary`. The server's download proxy
answers 404 for any other name (`server/app/download.go`,
`downloadableChannels`), and the agent's config rejects any other value for
`channel` (`internal/config/config.go`, `Validate`). `latest` is what every
installed agent follows and what the installer fetches by default; `canary`
is for the machine or two that take a build first. A host that publishes
only `latest/` serves every machine on the default channel.

Two rules for publishing into a channel:

- **Binaries first, manifests last.** Upload the four `loop-sessions_*`
  files, then `SHA256SUMS` and `latest.json`. A reader fetches the manifest
  and then the binary it names; a manifest published ahead of its binaries
  sends that reader to a 404 or, worse, to yesterday's bytes under today's
  digest, which the installer reports as a checksum mismatch.
- **Never cacheable.** Serve the channel with `Cache-Control: no-store` (the
  server proxy sets it on every response; the GCP example uploads with the
  same header). A `latest/` that a proxy holds for a day is a fleet installing
  yesterday's agent with no way to tell from the server.

A pattern the GCP example in `examples/deploy-gcp/` follows and any host can
copy: keep an immutable `builds/<sha>/` copy of every release beside the
channels, publish each merge to `canary/`, and promote to `latest/` by
copying a `builds/<sha>/` directory across (binaries first). Rollback is then
promoting an older directory, and the self-upgrade treats it exactly like a
forward move, because it compares bytes rather than version numbers.

## How `install.sh` consumes it

`install/install.sh` is POSIX `sh` with no dependency beyond `curl` or
`wget`, `awk`, `sed` and `shasum` or `sha256sum`. It reads four environment
variables. `LOOP_SESSIONS_BASE_URL` has no default: the script stops with a
message when it is unset, so a copy of it cannot quietly download from a host
nobody chose.

| Variable | Default | Purpose |
|---|---|---|
| `LOOP_SESSIONS_BASE_URL` | none; required (the script stops with a message when it is unset) | `<base>` above |
| `LOOP_SESSIONS_VERSION` | `latest` | the channel (`latest` or `canary`) |
| `LOOP_SESSIONS_BIN_DIR` | `~/.local/bin` | where the binary lands |
| `LOOP_SESSIONS_NO_ENROLL` | unset | `1` skips the sign-in hand-off |

and then, in order (`download_and_verify`, `install_binary`):

1. `GET <base>/<channel>/SHA256SUMS`. A failure here is "no such channel or
   no network" and stops everything.
2. Finds the line for `loop-sessions_<os>_<arch>` in it. No line means the
   release has no build for this platform, and the script says which.
3. `GET <base>/<channel>/loop-sessions_<os>_<arch>` into a temporary
   directory; an empty file is treated as a truncated transfer.
4. Computes the SHA-256 and compares it with the manifest's. On a mismatch
   the download is deleted, nothing is installed, and the exit is non-zero.
5. `GET <base>/<channel>/latest.json`, optionally. Failure is ignored; when
   it is there, `version` and `commit` are read with `sed` and printed after
   the install, so the line a person pastes into a bug report names a commit.
6. Stages the binary inside the target directory, `chmod 0755`, runs it once
   (`loop-sessions version`) and only then renames it over the old one. A
   build that passes its checksum and still does not start leaves the
   previous install untouched.

Every non-loopback fetch is HTTPS with `--proto '=https' --tlsv1.2` (curl) or
`--https-only` (wget), so a redirect cannot downgrade the connection.

The script ends by handing off to `loop-sessions install` for sign-in, which
is where the server address comes in: the binary uses its stamped
`defaultEndpoint` unless `--endpoint` is given. The release host and the
server are independent; only the binary's own stamp ties them.

To verify a download by hand instead, fetch `SHA256SUMS` and the binary for
your platform from `<base>/latest/` and run `shasum -a 256 -c SHA256SUMS
--ignore-missing` (macOS) or `sha256sum -c SHA256SUMS --ignore-missing`
(Linux). Because the build is reproducible, `make release` at the same commit
should give the same digest.

## How the in-app upgrade consumes it

The agent replaces its own binary from the daemon (`internal/upgrade`,
called from `cmd/loop-sessions/main.go`). What it reads, and from where:

- The base is **the server endpoint**, not the installer's
  `LOOP_SESSIONS_BASE_URL`: `upgrade.Run` is given `BaseURL: cfg.Endpoint`
  and reads `<endpoint>/dl/<channel>/SHA256SUMS`, then
  `<endpoint>/dl/<channel>/loop-sessions_<os>_<arch>`. It never reads
  `latest.json`. So auto-upgrade works only when the server itself answers
  `/dl/` (below); the installer works against any base.
- Staleness is a digest, not a version. The daemon hashes its own executable
  and compares it with the manifest's entry for its platform. Different
  bytes mean stale, whichever direction the version moved, so a rollback
  published to `latest/` propagates like any other release.
- The download is written to a temporary file in the same directory,
  `fsync`ed, checked against the digest, made executable, run once
  (`version`), and only then renamed over the running binary. Every failure
  leaves the old binary in place and is logged; none is fatal to capture.

The daemon checks at most once every six hours per machine
(`upgradeRecheck`), with a ten-minute budget per attempt (`upgradeBudget`).
`loop-sessions daemon --upgrade-now` runs one check immediately. The check
is skipped, and says why in the log and the health report, when:

- the binary is a development build (`Version` is `dev` or empty);
- `LOOP_SESSIONS_NO_UPGRADE` is set in the environment;
- `disable_auto_upgrade` is `true` in `~/.loop/sessions/config.json`;
- capture is paused (`--upgrade-now` only);
- the binary is outside the home directory, or its directory is not
  writable, on the reasoning that whatever installed it there owns it;
- the manifest lists no asset for this OS/architecture.

The channel comes from `channel` in the config (`latest` when unset).

## How the server serves and uses it

The server has two relationships with a release: it can serve one, and it
judges the fleet against one. Both are switched on by the `RELEASE_BUCKET`
environment variable and both currently assume Google Cloud Storage.

**Serving** (`server/app/download.go`). With `RELEASE_BUCKET` set, three
unauthenticated routes are mounted:

| Route | Serves |
|---|---|
| `GET /dl/{channel}/{asset}` | the object `<channel>/<asset>` from the bucket |
| `GET /install.sh` | the object `latest/install.sh` |
| `GET /install` | an HTML page with the one-liner `curl -fsSL <PUBLIC_URL>/install.sh \| sh`, a `/dl/latest/<binary>` link per platform and one to `/dl/latest/SHA256SUMS` |

`asset` must be one of the seven names in `downloadableAssets`
(`SHA256SUMS`, `install.sh`, `latest.json` and the four binaries) and
`channel` one of `latest` or `canary`; anything else is a plain 404, so the
route cannot be used to reach an arbitrary object. Objects are read with a
token from the instance metadata server, which is why this works on Cloud
Run or GCE and nowhere else, and every response carries `Cache-Control:
no-store`. With `RELEASE_BUCKET` unset the routes are not mounted at all, and
a deployment that distributes the agent another way answers 404 for `/dl/`.
The consequence for that deployment is the one stated above: installs work
from any base, and every laptop's auto-upgrade logs an error each check and
stays on its build.

**Judging** (`server/app/app.go`, `newManifestSource`; `server/fleet`). The
fleet page reads `latest/latest.json` from the same bucket, with the same
credential, decodes `version`, `commit`, `build_date` and `capture_schema`,
and compares the `commit` with the VCS stamp every machine sends in its
health report. That is how a machine is shown as behind, and how far. The
manifest is cached for five minutes. Without a bucket the evaluator reports
the manifest as missing once and judges nothing about versions.

## Hosting somewhere else, and GitHub Releases

Everything above reduces to one URL shape:

```
<base>/<channel>/SHA256SUMS
<base>/<channel>/latest.json
<base>/<channel>/loop-sessions_<os>_<arch>
```

Any static host that serves that shape over HTTPS is a complete release host
for the installer, with the two publishing rules (binaries first; no caching)
observed. What it does not give you is the server's `/dl/` proxy, and
therefore not auto-upgrade or the fleet page's version column; those are
bound to `RELEASE_BUCKET` and the GCS API today.

A GitHub Releases path is a welcome contribution. It is not there yet
because the URL shape differs: release assets live at
`.../releases/download/<tag>/<asset>`, with no channel directory and no
`latest` alias, and none of the three consumers knows that shape. The work,
as the code stands:

1. A workflow that runs `make release` and `make verify` on a tag and uploads
   `dist/*` as the release's assets. The existing GCP workflow in
   `examples/deploy-gcp/` shows the build and verification steps; only the
   upload differs.
2. Either a small redirector that maps `<base>/latest/<name>` onto the
   newest release's asset URL (no client change; the installer and the
   upgrade already follow HTTPS redirects), or a second URL shape taught to
   `install/install.sh` (`download_and_verify`) and `internal/upgrade`
   (`Run`, where `base` is built) and pinned by
   `internal/upgrade/upgrade_test.go`.
3. For auto-upgrade and the fleet page, a `ManifestSource` and a `/dl/`
   backend that read from an HTTPS base instead of a bucket
   (`server/app/app.go`, `newManifestSource`; `server/app/download.go`,
   `serve`), selected by a new environment variable so `RELEASE_BUCKET` keeps
   working for the GCP deployment.

If you take this on, keep the asset names and the two manifests exactly as
they are: every consumer, and the drift test, depends on them.
