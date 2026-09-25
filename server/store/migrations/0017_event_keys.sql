-- Cross-origin identity keys on events, and the one marker a hook row can carry.
--
-- A session captured both live (hooks) and from its transcript (the walker)
-- stores two rows for every prompt and every answer, and until now nothing on
-- the row said which two are the same moment: the prompt id the harness
-- stamps on both paths, the transcript record's own uuid, the assistant
-- message id, the tool_use id that pairs a call with its result. All of them
-- were inside body.raw, readable only by detoasting the body, which is why
-- every query that tried to fold the two copies together timed out. This file
-- gives each of them a column.
--
-- Columns only, every one nullable, no default: a catalog change on a table
-- of several million rows, no rewrite, no read. The values are filled two
-- ways. Ingest fills them for every new row from the event's named fields
-- when the client sent them and from body.raw otherwise (server/store
-- insertEvents), so old clients' rows are keyed from the day this ships. The
-- rows already stored are filled by the derive runner's event_keys step, in
-- keyset sub-batches of at most 5,000 rows under a 60 s statement timeout,
-- inside the derive window; that is the only step that reads bodies, and it
-- is minutes of work per night rather than one statement under the migration
-- lock.
--
-- No index here, deliberately. Each index on events is two full heap scans
-- (measured at 59-60 s each on the production clone, design.md section 11),
-- which a plain build cannot do without holding a SHARE lock against ingest
-- for the duration, and a concurrent build cannot run inside the transaction
-- Migrate wraps this file in. The runner builds them CONCURRENTLY as its own
-- recorded steps, in this order:
--   events_session_prompt_idx    ON events (session_id, prompt_id) WHERE prompt_id IS NOT NULL
--   events_record_identity_idx   ON events (session_id, coalesce(agent_id,''), record_uuid, type) WHERE record_uuid IS NOT NULL
--   events_superseded_idx        ON events (session_id, seq) WHERE superseded_by IS NOT NULL
--   messages_session_human_idx   ON messages (session_id, seq) WHERE role = 'user' AND kind IN ('human','slash_command') AND agent_id IS NULL
--   events_ingested_at_idx       ON events (ingested_at)
-- The fifth (added 2026-09-11) is for the export job's planning read and its
-- per-day COPY, which are range scans over ingested_at.
--
-- superseded_by names the row that stands in for this one in every read
-- that wants each moment once: the transcript copy of a prompt or an answer
-- the hook also captured. It is set on hook-origin rows only, and only by the
-- runner's fold of the row's own session, which also clears it on the hook
-- rows a later fold no longer pairs; ingest never writes the column. A
-- transcript row is never superseded, because the resume bundle is rebuilt
-- from transcript rows and a transcript with a hole in it cannot be resumed.
-- The events table stays append-only in every other column: bodies are
-- immutable and rows are never deleted by derivation.
--
-- Rollback floor: a revision from before this file neither reads nor writes
-- these columns. Its ingest inserts rows with every key NULL, which the next
-- runner pass fills; its reads return every row, superseded or not, which is
-- what they returned before. Nothing has to be undone.

ALTER TABLE events ADD COLUMN IF NOT EXISTS prompt_id          TEXT;
ALTER TABLE events ADD COLUMN IF NOT EXISTS record_uuid        TEXT;
ALTER TABLE events ADD COLUMN IF NOT EXISTS parent_record_uuid TEXT;
ALTER TABLE events ADD COLUMN IF NOT EXISTS request_id         TEXT;
ALTER TABLE events ADD COLUMN IF NOT EXISTS message_id         TEXT;
ALTER TABLE events ADD COLUMN IF NOT EXISTS tool_use_id        TEXT;
ALTER TABLE events ADD COLUMN IF NOT EXISTS superseded_by      TEXT;
