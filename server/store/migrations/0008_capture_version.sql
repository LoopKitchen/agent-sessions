-- Capture version: which extraction produced a stored event.
--
-- Without it, an agent that learns to extract something better from a transcript
-- cannot deliver the improvement. The event's id is derived from the record's
-- identity, so a re-walk produces the same id, and ON CONFLICT (id) DO NOTHING
-- discards the better copy without a trace. The version is what lets ingest tell
-- "this is the same event again" from "this is the same event, extracted by a
-- newer agent" — the first is discarded, the second replaces what is stored.
--
-- Zero is the default and means an agent that predates this column, which every
-- row does at the moment it is added. Zero is below every real version, so the
-- first re-walk by a versioned agent upgrades them.
ALTER TABLE events ADD COLUMN IF NOT EXISTS capture_version INTEGER NOT NULL DEFAULT 0;

-- Derivation version: which server-side derivation last ran over the corpus.
--
-- The server half of the same rule. Artifacts, links, messages and usage are all
-- functions of stored events, so when one of those functions changes, everything
-- derived under the old one is stale. This records what ran, so a server that
-- boots with newer derivation code can notice and rebuild once rather than
-- rebuilding on every revision rollout or, worse, never.
--
-- A single row, enforced by the primary key on a constant.
CREATE TABLE IF NOT EXISTS derived_schema (
  only_row     BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (only_row),
  version      INTEGER NOT NULL DEFAULT 0,
  rebuilt_at   TIMESTAMPTZ,
  -- What the last rebuild produced, so an operator reading this table can tell
  -- a rebuild that did something from one that silently found nothing.
  artifacts    INTEGER NOT NULL DEFAULT 0,
  links        INTEGER NOT NULL DEFAULT 0
);

-- Seeded at version 0 so the first boot of a server carrying derivation version
-- 1 sees a gap and rebuilds. Inserting nothing here would be indistinguishable
-- from a table that was never migrated.
INSERT INTO derived_schema (only_row, version) VALUES (TRUE, 0)
ON CONFLICT (only_row) DO NOTHING;
