# Upgrades reach the past, not just the future

The rule: **when a change makes this tool understand a session better, it applies
to the sessions already captured, not only to the ones captured next.**

A dashboard where the same transcript means different things depending on when it
happened to be read is a dashboard nobody can draw a conclusion from. Every
average is over a mixture of interpretations, every comparison between two weeks
is partly a comparison between two versions of the code, and nothing on the page
says so.

Three mechanisms carry it, each gated so that upgrades which change nothing about
interpretation cost nothing.

| Side | Constant or trigger | What it does | Where |
|---|---|---|---|
| Agent | `event.CaptureSchema` above `event.CaptureBaseline` | one full re-walk of this machine's transcripts | `startCaptureRewalk`, `cmd/loop-sessions/main.go` |
| Agent | `GET /v1/repair` (daily) | walks the sessions the server names, one file at a time | `runRepair`, `cmd/loop-sessions/repair.go` |
| Server | `store.DerivedSchema` | the derive runner recomputes keys, kinds, titles and turns for stored rows | `server/store/derive` |

All three are no-ops when nothing is stale, which is the common case. None blocks
anything a person is waiting on: the agent's walks run in the detached daemon,
and the server's derivation runs in the background after the service is serving.

## Capture version 4: no re-walk

Version 4 changes what every event carries (the anchors: `prompt_id`,
`tool_use_id`, `record_uuid`, `parent_record_uuid`; the fork lineage on a
child's `session_started`; the harness title; the launcher; capped tool output)
and, for the first time, changes which events some files emit (legacy
`agent-*.jsonl` files become their own streams; slash-command records carry the
person's words; compaction summaries without the flag are recognised by their
text). `CaptureBaseline` is 4 as well, so no machine re-walks for it. Two facts
make that the right call.

The server derives every one of these keys for the rows it already holds.
`events.body.raw` is the transcript record itself, so `record_uuid`,
`prompt_id`, `tool_use_id`, `request_id` and `message_id` are read off stored
rows by the derive runner (`event_keys` step) without any laptop sending
anything. A blanket re-walk of 4.8 million rows would rewrite bodies that
already contain the keys, with a higher number in one column, at nine minutes
of CPU and a few hundred megabytes per machine.

What a re-walk cannot do, the repair walk does. The two things the corpus is
missing that a transcript can supply are the final answers of 18,816 hook turns
captured by the old `Stop` handler and the tokens of Codex sessions stored
before capture v3 read `token_count`. Neither is fixed by a full re-walk: the
answers are hook-origin rows, which a transcript walk cannot upgrade, and usage
is credited from freshly inserted events, which upgraded rows are not. Both are
fixed by the server naming the sessions that need them (`GET /v1/repair`: hook-
only sessions with answerless turns, Codex sessions with zero tokens, owned by
the asking device) and the daemon walking exactly those files. Ingest recognises
each re-walked transcript event by its record identity and credits usage from
the whole admitted batch under the ledger's idempotent key, so a repair walk can
run twice and change nothing the second time.

## When to bump `event.CaptureSchema`

Bump it when the agent will extract something **better or different from the same
transcript**:

- a field that was being dropped is now read
- text that was being mangled or truncated is now read correctly
- a redaction rule that was letting a credential through is fixed
- a parser bug that mis-attributed events to the wrong agent or session

Do **not** bump it for:

- delivery, spool, daemon, CLI, install or upgrade changes; they extract nothing
- server-only changes; those are the other column
- anything only available live, that a transcript never contained
- logging, help text, comments

### The baseline

`event.CaptureBaseline` is the highest version whose arrival needs no re-walk. A
machine recorded below it adopts the current version and walks nothing.

It has been raised twice. Version 1 introduced the versioning and changed no
extraction: every machine installed before the field existed records 0, and
re-walking all of them would have spent nine minutes and a few hundred megabytes
each to rewrite rows with byte-identical bodies. Version 4 adds only what the
server derives for stored rows (above). Raise it alongside `CaptureSchema` only
when the versions in between changed nothing the server cannot derive, and say
so in the constant's comment.

### Changing which events are emitted

Sequence numbers are assigned in walk order and hashed into every event id
(`internal/backfill/backfill.go`, `push`). Emitting one extra event partway
through a session renumbers everything after it in that stream, so every later
event gets a new id. That used to make such a change unshippable: a re-walk
would insert a second copy of the rest of the session beside the old one.

It is shippable now because ingest identifies a transcript-origin event by
`(session_id, agent_id, record_uuid, type, tool_use_id)` before it looks at the
id. A re-walk that renumbers a file still names the same records; an incoming
event whose identity is already stored under a different id upgrades that row
(body, capture version) and its new id is acknowledged as a duplicate. The ids
still depend on walk order, and a file walked under two emission rules still
produces two id sets, but the second set lands on the first set's rows.

The costs that remain are real and are stated where the data is presented: a
record type that changes from one event type to another (a compaction summary
previously emitted as a prompt) is a new identity, so the old row stays and the
runner's turn fold is what stops it rendering twice; and the transcript walker
still does not emit `file_changed`, so artifacts exist only for sessions
captured live. Transcripts do contain the fields (`filePath`, `originalFile`,
`structuredPatch`, `newString`), so that extraction is a cost decision (it
stores the full before-text of every historical edit), not an id problem any
more.

### A re-walk cannot add usage to a stored event

A re-walk upgrades an event in place: same id, better body, higher
`capture_version`. That is enough to fix text, entrypoints, models, tool names
and anything else read straight off the row. It is **not** enough to fix tokens
under the ingest that credits usage from freshly inserted events only.

Usage is credited from `insertEvents`'s `xmax = 0` rows (`UpsertEvents` passes
just those to `creditUsage`, `server/store/store.go`), so an extraction that
starts reading token counts it used to drop reaches new sessions and new
machines, and leaves every session already stored at zero. Codex rollouts are
the known instance: `token_count` records went unread until capture v3, so the
790 Codex sessions captured before it have no tokens, and the v3 re-walk
re-delivered them as upgrades rather than as inserts.

The fix is server-side and is what the repair walk depends on: credit usage from
the whole admitted batch rather than from `fresh`, folding token deltas from what
the ledger actually returned. The ledger's key makes that idempotent. The key
is the message id alone: the table's primary key is still `(message_id,
request_id)` from the day the contract named both, but the server normalises
`request_id` to `""` at insert (`usageKey` in `server/store/store.go`), so every row, old and new, keys on the message id.

`Usage.RequestID` was withheld on both capture paths until that was true. From
the day the ledger keyed on both columns, every stored Claude row carried an
empty `request_id`, and a client that filled the field in would have given the
same call a second key on its next repair walk and billed it again; the capture
contract recorded that as a waiver and the tests pinned the field empty. With the server-side
normalisation live the waiver's condition is met, and the current client
stamps the record's `requestId` on the walked copy (the walker, at
the record) and on the live copy (`LastAssistantRecord`, at the tail), so the
hook copy and the transcript copy of one call carry the same message id and
request id, and `events.request_id` holds the value as a stored column (the
analytics export and ad hoc queries read it; the derive fold still pairs the
two copies by message id) without a read of `body.raw`. No walker version bump: the derive runner's
`event_keys` step already read `requestId` out of `body.raw` for every stored
transcript row whose body still holds the raw record (a body removed by
retention, or a value over `KeyMaxBytes`, has none either way), so a re-walk
would add nothing; only hook copies gain the column, and only from this client
on.

## When to bump `store.DerivedSchema`

Bump it when the server will derive something **better or different from events
already stored**:

- a new derived table
- a changed classification rule (what counts as a PR link, how a path is parsed,
  which user records are human)
- a corrected checksum or size, as in the artifact snapshot fix
- a derived field that was being computed wrongly

Do **not** bump it for read-path, template, routing, auth or styling changes.
Those render what is already derived; they do not change it, and a rebuild would
spend minutes producing byte-identical rows.

The server side is cheaper and safer than the client side, because it needs no
network, touches no one's laptop, and events are the source of truth: everything
in `artifacts`, `artifact_versions`, `links`, `messages` and `turns` is a pure
function of `events` and can be discarded and recomputed at any time. Prefer
moving interpretation to the server for exactly this reason; version 4 is the
worked example.

### Forcing a rebuild of the current version

The runner records every finished step in `derive_jobs` and re-stamps
`derived_schema` when no step of the current version is pending, so lowering
the stamp alone does nothing: the next pass finds every step finished and
stamps again. Each step names the version that introduced its rule
(`deriveStep.since` in `server/store/runner.go`; the thirteen steps of
version 3 carry 3, `skill_invocations` carries 4), and `ensureDeriveJobs`
seeds a step finished when the stored version is at or past its `since`.
That is what makes a bump for one new step run that step alone: a stored 3
seeds the thirteen earlier steps finished and runs `skill_invocations`,
which is night one of version 4; a stored 0 (a fresh database) or 2 runs
all fourteen.

So there are two forced rebuilds of version `N` (the value of
`store.DerivedSchema`), both two statements in this order. The steps new in
`N` only:

```sql
DELETE FROM derive_jobs WHERE version = N;
UPDATE derived_schema SET version = N - 1 WHERE only_row;
```

Every step, from the start:

```sql
DELETE FROM derive_jobs WHERE version = N;
UPDATE derived_schema SET version = 0 WHERE only_row;
```

The running instance notices at its next check (six hours after it last saw
the stamp) or at its next boot, and runs the versioned pass again: with the
stamp at `N - 1` the steps the stored version already covers are seeded
finished and only the new ones run; with the stamp at 0 the index steps find
their indexes valid and finish at once, and `event_keys` and the steps after
it re-derive every row inside the derive window. The targeted repair of the
`skill_invocations` step alone is an admin route rather than SQL:
`POST /v1/admin/derive/skill-invocations/rerun` (`examples/deploy-gcp/README.md`,
"Runbook: skill invocations") clears that step's row, lowers the stamp to
`N - 1` (never raising one already below it) and writes an audit row; with a `since` it queues the sessions of a
window for the dirty tick instead and changes no stamp. One parked step is
a different case and needs no reset of the stamp:
its `derive step failed` line carries the `UPDATE derive_jobs SET attempts = 0
...` statement that resumes it.

## How an improved event actually replaces a stored one

Event ids are deterministic, so a re-walk produces the same id for the same
record. `ON CONFLICT (id) DO NOTHING` would therefore discard the improvement
silently: the re-walk would run for nine minutes and change nothing.

Events carry `capture_version`, and ingest keeps the highest:

```sql
ON CONFLICT (id) DO UPDATE SET body = EXCLUDED.body, ...
WHERE EXCLUDED.capture_version > events.capture_version
```

A re-delivery carries the same version, the `WHERE` fails, and the row is neither
written nor returned, identical to the old behaviour. Only a strictly newer
extraction replaces what is stored. Record identity (above) is the second door:
an event whose identity is stored under another id takes that row's place by
the same rule.

`RETURNING id, (xmax = 0) AS inserted` separates the two outcomes, and that
separation was load-bearing while usage was credited from fresh rows only. It
stays informative once crediting moves to the ledger key: an upgraded row
refreshes the event, and everything derived from it is rebuilt by the runner.

## Discretion

The rule is "when it makes the tool understand a session better", not "on every
release". A re-walk is around nine minutes of CPU and a few hundred megabytes on
the wire per machine on the reference corpus; a rebuild is under a minute today
and grows with the corpus. Neither is free, and both are entirely wasted when the
change did not alter interpretation.

The honest test before bumping either constant:

> If I ran this build over last month's sessions, would anything about them be
> different, and would that difference change what somebody concluded?

Two noes means leave the version alone. A yes to the first and a no to the second
usually means leave it alone too, and say so in the commit message so the next
person does not wonder whether it was an oversight. A yes to both with the
server able to derive the difference from stored rows means bump `CaptureSchema`
and `CaptureBaseline` together, which is what version 4 did.
