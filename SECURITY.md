# Security policy

loop-sessions reads coding-session transcripts on people's laptops and uploads
them to a server. A vulnerability here can expose a colleague's work, so
reports are taken seriously and handled privately until a fix is released.

## Reporting a vulnerability

Do not open a public issue for a security problem.

1. Preferred: use GitHub's private vulnerability reporting. Open the
   repository's **Security** tab and choose **Report a vulnerability**. The
   report is visible only to the maintainers until it is published.
2. Otherwise, email `security@loopai.com`.

Include what you found, how to reproduce it, and which component it is in
(client agent, installer, server, dashboard, an example deployment). A
proof-of-concept helps; a working exploit against someone else's deployment
does not, so please test only against a server you run.

You will get an acknowledgement, then a fix or a reasoned response. We will
credit you in the fix's commit message and in AUTHORS.md unless you ask us not
to.

## Supported versions

Only the `main` branch is supported. Fixes land on `main`; there are no
maintained release branches. Self-hosted deployments should track `main` and
rebuild.

## Bug bounty

There is no bug bounty programme.

## What is in scope

- The client agent (`cmd/loop-sessions`, `internal/`): anything that lets a
  hook affect the harness it runs in, lets a local process obtain the device
  credential, or gets credential material past the scrubber
  (`internal/scrub`) and onto the wire.
- The installer (`install/`): anything that weakens checksum verification or
  the HTTPS-only fetch.
- The server (`server/`): authentication and authorization (a read of a
  session the viewer may not see, an admin route reachable by a member, a
  device token accepted for the wrong person), the ingest endpoints, and
  anything that renders transcript content as live HTML or script in the
  dashboard.

Findings in the example deployment under `examples/deploy-gcp/` are welcome
too, with the caveat that it is an example rather than a supported product.
