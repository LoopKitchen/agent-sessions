-- Session type: automation vs user-initiated.
--
-- The signal lives in transcript-origin events: the transcript line's
-- top-level "entrypoint" field says how the harness was started ("cli" and
-- "claude-desktop" are a person; "sdk-cli" is headless -p / SDK, which is how
-- cron jobs and pipelines run). Hook-origin events predating capture schema 2
-- carry no such field; sessions only ever captured live before that schema are
-- corrected by the DerivedSchema-2 reclassify after the fleet re-walk lands
-- their transcript copies. The backfill below reads it as a jsonb path,
-- never as a substring — a session whose TOOL OUTPUT quotes another transcript
-- contains the literal text "entrypoint":"sdk-cli" inside a string, and a
-- substring match would classify the person reading a cron log as a robot.
--
-- Codex rollouts carry no entrypoint; their session_meta payload's originator
-- names the launcher, and codex exec runs identify themselves there. Anything
-- with no signal at all stays 'user': automation must be proven, because the
-- default view hides it.

ALTER TABLE sessions ADD COLUMN IF NOT EXISTS session_type text NOT NULL DEFAULT 'user'
  CHECK (session_type IN ('user', 'automation'));

UPDATE sessions s SET session_type = 'automation'
WHERE s.session_type <> 'automation'
  AND EXISTS (
  SELECT 1 FROM events e
  WHERE e.session_id = s.session_id
    AND (e.body->'raw'->>'entrypoint' = 'sdk-cli'
         OR e.body->'raw'->'payload'->>'originator' LIKE 'codex_exec%')
);

CREATE INDEX IF NOT EXISTS sessions_type_started_at_idx ON sessions (session_type, started_at DESC);
