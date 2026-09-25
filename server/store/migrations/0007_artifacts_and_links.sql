-- Artifacts and links: what a session produced, as opposed to what it said.
--
-- Both are derived from events that are already stored. Nothing here is a new
-- thing leaving anybody's machine: a file_changed event already carries the
-- path and the full before/after text, and a link is already sitting in the
-- message text. What did not exist was any way to ask "which files did this
-- session change" or "which PR came out of it" without reading a transcript
-- end to end, and no way at all to ask it across sessions.
--
-- Derivation happens in the same transaction as the events it derives from, so
-- these tables cannot describe a session the events table does not.

-- One row per (session, path). The session scope is deliberate: the same file
-- edited in two sessions is two artifacts, because the interesting question is
-- almost always "what did THIS session do to it", and the cross-session view is
-- a query over path rather than a shared row that neither session owns.
CREATE TABLE IF NOT EXISTS artifacts (
  id             BIGSERIAL PRIMARY KEY,
  session_id     TEXT NOT NULL REFERENCES sessions(session_id) ON DELETE CASCADE,
  email          TEXT NOT NULL,
  path           TEXT NOT NULL,
  -- Whether this session brought the file into existence, which is a different
  -- claim from "the first version we saw had no predecessor".
  created        BOOLEAN NOT NULL DEFAULT FALSE,
  first_seen_at  TIMESTAMPTZ NOT NULL,
  last_seen_at   TIMESTAMPTZ NOT NULL,
  -- Counted rather than derived at read time: the session list renders this for
  -- every row, and a COUNT per artifact per page is the kind of query that is
  -- fine at 93 artifacts and not fine at a fleet's worth.
  version_count  INTEGER NOT NULL DEFAULT 0,
  latest_sha256  TEXT NOT NULL DEFAULT '',
  latest_bytes   BIGINT NOT NULL DEFAULT 0,
  UNIQUE (session_id, path)
);

-- The session page lists a session's artifacts newest-touched first.
CREATE INDEX IF NOT EXISTS artifacts_session_last_seen_idx
  ON artifacts (session_id, last_seen_at DESC);
-- "every session that touched this file", which is the cross-session question
-- the per-session rows above cannot answer on their own.
CREATE INDEX IF NOT EXISTS artifacts_path_last_seen_idx
  ON artifacts (path, last_seen_at DESC);
CREATE INDEX IF NOT EXISTS artifacts_email_last_seen_idx
  ON artifacts (email, last_seen_at DESC);

-- One row per content state the file passed through.
--
-- Keyed by the event that produced it, not by the checksum: the same content can
-- legitimately recur when an edit is reverted, and collapsing those into one row
-- would lose the fact that it happened twice. Consecutive identical checksums
-- are skipped at write time instead, which is what makes this a history of
-- changes rather than a log of writes.
CREATE TABLE IF NOT EXISTS artifact_versions (
  artifact_id  BIGINT NOT NULL REFERENCES artifacts(id) ON DELETE CASCADE,
  event_id     TEXT NOT NULL REFERENCES events(id) ON DELETE CASCADE,
  sha256       TEXT NOT NULL,
  bytes        BIGINT NOT NULL,
  created      BOOLEAN NOT NULL DEFAULT FALSE,
  occurred_at  TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (artifact_id, event_id)
);

-- Version history is read newest-first, and the content itself is fetched from
-- the event body via event_id rather than copied here. Storing the text twice
-- would double the largest thing in the database to save one join.
CREATE INDEX IF NOT EXISTS artifact_versions_history_idx
  ON artifact_versions (artifact_id, occurred_at DESC);

-- One row per (session, url).
--
-- url is bounded by the client-side MaxURL (2048) before it ever reaches here,
-- because a btree entry over roughly 2,700 bytes is rejected and that rejection
-- would fail the whole ingest batch rather than just the link.
CREATE TABLE IF NOT EXISTS links (
  id             BIGSERIAL PRIMARY KEY,
  session_id     TEXT NOT NULL REFERENCES sessions(session_id) ON DELETE CASCADE,
  email          TEXT NOT NULL,
  url            TEXT NOT NULL,
  -- pr, issue, commit, repo, doc, slack, other.
  kind           TEXT NOT NULL,
  host           TEXT NOT NULL,
  -- The short human name: owner/repo#4. Empty when the URL is its own best name.
  ref            TEXT NOT NULL DEFAULT '',
  first_seen_at  TIMESTAMPTZ NOT NULL,
  last_seen_at   TIMESTAMPTZ NOT NULL,
  occurrences    INTEGER NOT NULL DEFAULT 1,
  UNIQUE (session_id, url)
);

CREATE INDEX IF NOT EXISTS links_session_kind_idx ON links (session_id, kind);
-- "every session that touched this PR", and the fleet-wide "what shipped this
-- week" view, both of which read by kind and recency.
CREATE INDEX IF NOT EXISTS links_kind_last_seen_idx ON links (kind, last_seen_at DESC);
CREATE INDEX IF NOT EXISTS links_email_last_seen_idx ON links (email, last_seen_at DESC);
CREATE INDEX IF NOT EXISTS links_ref_idx ON links (ref) WHERE ref <> '';
