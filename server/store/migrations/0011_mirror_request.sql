-- The per-session Slack mirror request, on the rollup where the mirror reads.
--
-- Stamped from the SessionStarted event's mirror_request field at ingest. It
-- records what the LAUNCHING SHELL asked for; whether anything posts is
-- decided by intersecting this with the owner's slack_prefs row at mirror
-- time, because an environment variable is not consent and the person's
-- server-side preference always wins. Set-once: the request describes how the
-- session was started, and later batches carrying nothing must not clear it.

ALTER TABLE sessions ADD COLUMN IF NOT EXISTS mirror_request TEXT NOT NULL DEFAULT '';

-- The mirror's live pass scans for sessions that asked. Partial: almost every
-- session never asks, and the index should be the size of the feature's use,
-- not the corpus.
CREATE INDEX IF NOT EXISTS sessions_mirror_request_idx
  ON sessions (started_at DESC) WHERE mirror_request <> '';
