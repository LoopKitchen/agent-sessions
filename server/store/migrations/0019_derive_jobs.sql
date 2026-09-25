-- The derive runner's ledger: one row per (version, step), with its cursor.
--
-- The previous rebuild was a single version stamp and a goroutine that
-- walked every event with text into memory at boot: on a 512 MiB container
-- over five million rows that is an OOM, not a slow run, and a rolling
-- deploy started it on every instance at once because the stamp was only
-- written at the end. A rebuild that has to survive a deploy mid-way, on
-- several instances, over a corpus that no longer fits in memory, needs a
-- cursor per step that commits with the step's own work. That is this table.
--
-- cursor is the keyset position the step has committed up to: the last
-- session_id for the per-session steps, "session_id<US>seq<US>id" (unit
-- separator, 0x1f) for the event_keys sub-batches, empty at the start. attempts counts failed
-- batches; a step that fails five times stops the pass and stays visible
-- here with last_error, which is what the derive_step_failed alert reads.
-- finished_at is set once and only once the step's work is complete;
-- derived_schema.version is stamped only when every step of the version
-- carries one, so a half-finished pass never reads as done.
--
-- Progress query for the runbook: SELECT * FROM derive_jobs WHERE version =
-- (SELECT max(version) FROM derive_jobs) ORDER BY ordinal.
--
-- Rollback floor: a revision from before this file ignores the table. Its
-- rows are left as they are and the newer revision's runner resumes from the
-- cursors it finds. Nothing is lost.

CREATE TABLE IF NOT EXISTS derive_jobs (
  version     INT  NOT NULL,
  step        TEXT NOT NULL,
  ordinal     INT  NOT NULL,
  cursor      TEXT NOT NULL DEFAULT '',
  processed   BIGINT NOT NULL DEFAULT 0,
  attempts    INT NOT NULL DEFAULT 0,
  started_at  TIMESTAMPTZ,
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  finished_at TIMESTAMPTZ,
  last_error  TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (version, step)
);
