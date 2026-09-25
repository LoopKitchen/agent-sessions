-- The session classification lattice, lineage columns and the head-state
-- column.
--
-- session_type grows from {user, automation} to the lattice
-- empty < user < internal < automation. empty is a session that has only ever
-- shown lifecycle events (a GUI host spawning and discarding a CLI process,
-- see research/r8); internal is a session whose opening prompt is one of the
-- harness's own templates (the title generator, the status-line labeler, the
-- liveness ping). Both leave the default views; neither is deleted.
--
-- session_type_merge is the ONLY merge rule, called from the ingest rollup and
-- from the evidence-only mark path. It is a rank merge with one asymmetry: a
-- session can only be BORN empty. Once it has been anything else it never
-- returns to empty, however many content-free batches (a late session_ended,
-- a compaction, a reconcile pass) arrive after it. The pre-existing rows all
-- have content_events = 0 at the moment this column appears, which is why the
-- backfill below sets content_events from the counters that were already
-- being kept before the merge function can ever see a zero, and why the
-- function itself refuses to demote a user row even when it does.
--
-- internal is not merged in from ingest deltas at all: it is recomputed from
-- the session's opening prompt on every touched batch, because batches arrive
-- in any order and a template arriving before the real opening prompt must
-- not stick. The function still knows the value so a stored internal row
-- survives later merges, and so automation absorbs it.
--
-- empty_kind refines an empty row (aborted, blank, head_truncated,
-- tail_truncated) and is cleared the moment the row is promoted. It is
-- provisional at ingest and authoritative after the derive runner's
-- session_class step.
--
-- content_events counts the events that mean something happened: prompts,
-- turns, tools, file changes, subagents. Lifecycle markers do not count.
--
-- entrypoint, launcher and transcript_exists keep the launcher facts a
-- session's events carried, so a per-machine "broken automation" alert can say
-- how the empties were started. entrypoint is set once and survives the type.
--
-- parent_record_uuid and lineage_source end the lineage fiction. The walker
-- has been stamping logicalParentUuid, which is the uuid of the last record
-- before a compaction INSIDE THE SAME FILE, into parent_session_id, so every
-- compacted session claimed a parent that no session ever matched (384 rows,
-- 0 resolvable). The marker moves to parent_record_uuid, where it means what
-- it is, and parent_session_id may only be written together with a
-- lineage_source that says how it was proven. The UPDATE is gated on
-- lineage_source = '' so it is safe to re-run after any rollback: a parent
-- written by a newer revision with a source is never touched.
--
-- head_state ships now, computed later. The default list filter needs the
-- column from the first day of the lattice so that a session whose device
-- reported capture loss (head_state = 'capture_loss') is never hidden as
-- "nothing happened"; the derive runner sets the value.
--
-- derive_dirty is the runner's work queue: ingest sets it on every touched
-- session and the runner clears it when the session's derived rows are current.
-- It defaults to true so every existing row is folded once.
--
-- Cost: every statement is catalog-only except the two UPDATEs over sessions
-- (thousands of rows, no read of events). The CHECK constraints are validated
-- against sessions alone.
--
-- Rollback floor: a revision from before this file writes only 'user' and
-- 'automation' and passes the widened CHECK. Its rollup assigns the type
-- outright on every fold, so a row classified empty or internal here that
-- the old revision touches reverts to 'user' and is hidden again only once
-- the newer revision's derive runner re-derives it; rows it does not touch
-- keep their value. Nothing is lost either way: the classification is
-- recomputable from the event log, and the columns this file adds sit
-- unused. The fiction UPDATE is idempotent as described above.

ALTER TABLE sessions DROP CONSTRAINT IF EXISTS sessions_session_type_check;
ALTER TABLE sessions ADD CONSTRAINT sessions_session_type_check
  CHECK (session_type IN ('empty', 'user', 'internal', 'automation'));

-- A row is born empty. Ingest claims the row before it folds the batch that
-- created it, and the claim writes only the identity columns, so the type a
-- row starts with is this default; with the old default of 'user' the merge
-- below would see a user row on the very first fold and refuse to call it
-- empty, and no lifecycle-only session could ever be classified at ingest.
-- Existing rows are untouched (a default is not a rewrite); a revision from
-- before this file assigns 'user' or 'automation' outright on every fold, so
-- the default is invisible to it.
ALTER TABLE sessions ALTER COLUMN session_type SET DEFAULT 'empty';

ALTER TABLE sessions ADD COLUMN IF NOT EXISTS empty_kind TEXT
  CHECK (empty_kind IS NULL OR empty_kind IN ('aborted', 'blank', 'head_truncated', 'tail_truncated'));

ALTER TABLE sessions ADD COLUMN IF NOT EXISTS content_events INT NOT NULL DEFAULT 0;
UPDATE sessions SET content_events = user_turns + tool_calls + subagents WHERE content_events = 0;

ALTER TABLE sessions ADD COLUMN IF NOT EXISTS entrypoint TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS launcher JSONB;
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS transcript_exists BOOLEAN;

ALTER TABLE sessions ADD COLUMN IF NOT EXISTS parent_record_uuid TEXT;
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS lineage_source TEXT NOT NULL DEFAULT '';
UPDATE sessions SET parent_record_uuid = parent_session_id, parent_session_id = NULL
 WHERE parent_session_id IS NOT NULL
   AND lineage_source = ''
   AND NOT EXISTS (SELECT 1 FROM sessions p WHERE p.session_id = sessions.parent_session_id);

ALTER TABLE sessions ADD COLUMN IF NOT EXISTS head_state TEXT NOT NULL DEFAULT 'unknown'
  CHECK (head_state IN ('unknown', 'complete', 'truncated_window', 'start_lost', 'capture_loss'));

ALTER TABLE sessions ADD COLUMN IF NOT EXISTS derive_dirty BOOLEAN NOT NULL DEFAULT true;

CREATE OR REPLACE FUNCTION session_type_merge(old TEXT, new TEXT) RETURNS TEXT
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
  SELECT CASE
    WHEN old IS NULL THEN new
    WHEN 'automation' IN (old, new) THEN 'automation'
    WHEN 'internal' IN (old, new) THEN 'internal'
    WHEN old = 'empty' THEN new
    ELSE 'user'
  END
$$;
