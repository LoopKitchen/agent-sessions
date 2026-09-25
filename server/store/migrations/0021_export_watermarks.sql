-- The export job's watermarks: one row per stream it copies out.
--
-- The hourly export (server/export, run as the Cloud Run job
-- loop-sessions-export) copies the derived tables to Cloud Storage and loads
-- them into BigQuery. It has to know where the last successful run stopped,
-- and that position has to live in the database it reads from rather than in
-- the job's own filesystem, which Cloud Run discards between executions.
--
-- Three rows, by name: sessions (the sessions.updated_at the session-keyed
-- tables were exported up to; turns, sessions and messages share it because
-- they are re-exported by the session's start day), events (ingested_at) and
-- health_hourly (hour). A row is written only after every load job of a run
-- succeeded, so a run that failed half-way leaves the position where it was
-- and the next run repeats the work; the partitions are rewritten whole
-- (WRITE_TRUNCATE), so repeating is safe. A missing row means "from the
-- beginning", which is what the first run wants and what an operator who
-- wants a full re-export deletes to get.
--
-- Catalog-only: a new table with no rows. Nothing reads events or messages.
--
-- Rollback floor: a revision from before this file never reads the table and
-- the job that writes it is a separate process; the rows sit unused until the
-- job returns. Nothing is lost.

CREATE TABLE IF NOT EXISTS export_watermarks (
  name       TEXT PRIMARY KEY,
  watermark  TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
