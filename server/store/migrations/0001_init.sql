-- Initial schema for loop-sessions.
--
-- Every statement is written to be safe to re-run. Servers start concurrently
-- during a rolling deploy and each one runs migrations, so a migration that is
-- only correct on an empty database is a migration that eventually takes the
-- fleet down at the worst possible moment.
--
-- The shape follows the server contract. Two rules run through all of it. Event
-- time and ingest time are separate columns, so a session backfilled from a
-- laptop's history is indistinguishable from one captured live except for when
-- it arrived. And the events table is the durable record: sessions is a rollup
-- that can always be rebuilt from events, which is why nothing else depends on
-- it being correct at any particular instant.

-- Who may use the system, and at what level. The first admins are written at
-- boot from the ADMIN_EMAILS environment variable (server/app/config.go,
-- store.BootstrapAdmins) and everything after that is edited through the
-- admin page; no migration seeds a person. Roles live here rather than in a
-- directory service because the alternative was domain-wide delegation against
-- Google Workspace, and a table we own is one fewer approval and one fewer
-- nightly sync that can silently stop.
CREATE TABLE IF NOT EXISTS principals (
  email        TEXT PRIMARY KEY,
  role         TEXT NOT NULL CHECK (role IN ('admin','member')),
  display_name TEXT,
  added_by     TEXT,
  added_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  disabled_at  TIMESTAMPTZ
);

-- A machine enrolled by a person. One row per (person, device).
CREATE TABLE IF NOT EXISTS devices (
  id            UUID PRIMARY KEY,
  email         TEXT NOT NULL REFERENCES principals(email),
  hostname      TEXT,
  os            TEXT,
  arch          TEXT,
  agent_version TEXT,
  enrolled_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_seen_at  TIMESTAMPTZ,
  revoked_at    TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS devices_email_idx ON devices (email) WHERE revoked_at IS NULL;

-- Device credentials. Only the hash is stored; the token itself is shown once.
CREATE TABLE IF NOT EXISTS device_tokens (
  id           UUID PRIMARY KEY,
  device_id    UUID NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  token_hash   BYTEA NOT NULL,
  issued_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at   TIMESTAMPTZ,
  revoked_at   TIMESTAMPTZ,
  last_used_at TIMESTAMPTZ
);
CREATE UNIQUE INDEX IF NOT EXISTS device_tokens_token_hash_idx
  ON device_tokens (token_hash) WHERE revoked_at IS NULL;

-- A captured session. Derived from events and rebuildable from them, so this is
-- a materialised rollup rather than a second source of truth.
CREATE TABLE IF NOT EXISTS sessions (
  session_id         TEXT PRIMARY KEY,
  email              TEXT NOT NULL REFERENCES principals(email),
  device_id          UUID REFERENCES devices(id),
  source             TEXT NOT NULL,
  parent_session_id  TEXT,
  cwd                TEXT,
  repo               TEXT,
  git_branch         TEXT,
  started_at         TIMESTAMPTZ NOT NULL,
  ended_at           TIMESTAMPTZ,
  ended              BOOLEAN NOT NULL DEFAULT false,
  user_turns         INT NOT NULL DEFAULT 0,
  tool_calls         INT NOT NULL DEFAULT 0,
  subagents          INT NOT NULL DEFAULT 0,
  errors             INT NOT NULL DEFAULT 0,
  first_prompt       TEXT,
  harness_versions   TEXT[],
  tokens_input       BIGINT NOT NULL DEFAULT 0,
  tokens_output      BIGINT NOT NULL DEFAULT 0,
  tokens_cache_read  BIGINT NOT NULL DEFAULT 0,
  tokens_cache_write BIGINT NOT NULL DEFAULT 0,
  cost_usd           NUMERIC(12,6) NOT NULL DEFAULT 0,
  redactions         JSONB,
  ingested_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS sessions_email_started_at_idx ON sessions (email, started_at DESC);
CREATE INDEX IF NOT EXISTS sessions_started_at_idx ON sessions (started_at DESC);
CREATE INDEX IF NOT EXISTS sessions_parent_session_id_idx
  ON sessions (parent_session_id) WHERE parent_session_id IS NOT NULL;

-- One row per event. This is the durable record; sessions is derived from it.
-- There is deliberately no foreign key to sessions: events must be storable the
-- instant they arrive, and making the durable record depend on the rollup being
-- present first would invert which of the two is authoritative.
CREATE TABLE IF NOT EXISTS events (
  id          TEXT PRIMARY KEY,
  session_id  TEXT NOT NULL,
  email       TEXT NOT NULL,
  seq         BIGINT NOT NULL,
  type        TEXT NOT NULL,
  origin      TEXT NOT NULL,
  occurred_at TIMESTAMPTZ NOT NULL,
  ingested_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  agent_id    TEXT,
  workflow_id TEXT,
  model       TEXT,
  tool_name   TEXT,
  body        JSONB NOT NULL
);
CREATE INDEX IF NOT EXISTS events_session_id_seq_idx ON events (session_id, seq);
CREATE INDEX IF NOT EXISTS events_email_occurred_at_idx ON events (email, occurred_at DESC);

-- Full-text search at MESSAGE granularity. Session-level indexing is impossible:
-- a tsvector is capped at 1 MB and sessions routinely exceed that.
CREATE TABLE IF NOT EXISTS messages (
  event_id    TEXT PRIMARY KEY REFERENCES events(id) ON DELETE CASCADE,
  session_id  TEXT NOT NULL,
  email       TEXT NOT NULL,
  seq         BIGINT NOT NULL,
  role        TEXT NOT NULL,
  occurred_at TIMESTAMPTZ NOT NULL,
  text        TEXT NOT NULL,
  tsv         tsvector GENERATED ALWAYS AS (to_tsvector('english', text)) STORED
);
CREATE INDEX IF NOT EXISTS messages_tsv_idx ON messages USING GIN (tsv);
CREATE INDEX IF NOT EXISTS messages_session_id_seq_idx ON messages (session_id, seq);
-- Search filters on owner and recency before it ranks anything, so the ordering
-- that produces the capped candidate set has to be servable from an index.
-- Without this the cap does not save the query, it only truncates it after the
-- expensive part has already run.
CREATE INDEX IF NOT EXISTS messages_email_occurred_at_idx ON messages (email, occurred_at DESC);

-- Explicit per-session grants, on top of the role model.
CREATE TABLE IF NOT EXISTS shares (
  id         UUID PRIMARY KEY,
  session_id TEXT NOT NULL REFERENCES sessions(session_id) ON DELETE CASCADE,
  created_by TEXT NOT NULL,
  grantee    TEXT,
  token      TEXT UNIQUE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at TIMESTAMPTZ,
  revoked_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS shares_session_id_idx ON shares (session_id) WHERE revoked_at IS NULL;

-- Every read of someone else's session. Required because the whole premise of
-- capturing colleagues' work rests on the access being auditable.
CREATE TABLE IF NOT EXISTS access_log (
  id         BIGSERIAL PRIMARY KEY,
  viewer     TEXT NOT NULL,
  session_id TEXT NOT NULL,
  owner      TEXT NOT NULL,
  via        TEXT NOT NULL,
  at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS access_log_session_id_at_idx ON access_log (session_id, at DESC);
CREATE INDEX IF NOT EXISTS access_log_viewer_at_idx ON access_log (viewer, at DESC);

-- Client self-telemetry. The fleet view and all alerting derive from this.
CREATE TABLE IF NOT EXISTS health_reports (
  id          BIGSERIAL PRIMARY KEY,
  email       TEXT NOT NULL,
  device_id   UUID,
  emitted_at  TIMESTAMPTZ NOT NULL,
  received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  worst       TEXT NOT NULL,
  report      JSONB NOT NULL
);
CREATE INDEX IF NOT EXISTS health_reports_email_emitted_at_idx ON health_reports (email, emitted_at DESC);
-- Health delivery is at-least-once like everything else the client sends, and a
-- report has no natural idempotency key of its own. The emitting machine plus
-- the moment it sampled is that key, so a retried report lands once. NULLS NOT
-- DISTINCT matters here because a device that has not finished enrolling still
-- reports, with a null device id.
CREATE UNIQUE INDEX IF NOT EXISTS health_reports_idempotency_idx
  ON health_reports (email, device_id, emitted_at) NULLS NOT DISTINCT;

-- Token usage that has been counted, keyed by the identity of the model call
-- rather than by the event that carried it.
--
-- The same assistant message legitimately appears in more than one transcript
-- record, and those records are genuinely different events with different ids,
-- so event-level idempotency does not stop the usage inside them being counted
-- twice. Nothing in the session rollup is credited until the row lands here, and
-- the primary key is what makes that credit exactly-once across batches, across
-- retries and across a backfill that overlaps live capture.
CREATE TABLE IF NOT EXISTS usage_ledger (
  message_id            TEXT NOT NULL,
  request_id            TEXT NOT NULL,
  event_id              TEXT NOT NULL,
  session_id            TEXT NOT NULL,
  email                 TEXT NOT NULL,
  model                 TEXT NOT NULL,
  occurred_at           TIMESTAMPTZ NOT NULL,
  ingested_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
  input_tokens          BIGINT NOT NULL DEFAULT 0,
  output_tokens         BIGINT NOT NULL DEFAULT 0,
  cache_read_tokens     BIGINT NOT NULL DEFAULT 0,
  cache_write_5m_tokens BIGINT NOT NULL DEFAULT 0,
  cache_write_1h_tokens BIGINT NOT NULL DEFAULT 0,
  cost_usd              NUMERIC(12,6) NOT NULL DEFAULT 0,
  PRIMARY KEY (message_id, request_id)
);
CREATE INDEX IF NOT EXISTS usage_ledger_session_id_idx ON usage_ledger (session_id);

-- Model rates with effective dates. Cost is computed once at ingest and stored,
-- so a rate change must not silently rewrite what last quarter cost; keeping the
-- rates as dated rows rather than as constants in a binary is what lets a stored
-- figure be re-derived and defended later.
CREATE TABLE IF NOT EXISTS model_prices (
  model                     TEXT NOT NULL,
  effective_from            TIMESTAMPTZ NOT NULL,
  input_per_mtok            NUMERIC(12,6) NOT NULL,
  output_per_mtok           NUMERIC(12,6) NOT NULL,
  cache_read_multiplier     NUMERIC(8,4) NOT NULL DEFAULT 0.1,
  cache_write_5m_multiplier NUMERIC(8,4) NOT NULL DEFAULT 1.25,
  cache_write_1h_multiplier NUMERIC(8,4) NOT NULL DEFAULT 2.0,
  PRIMARY KEY (model, effective_from)
);

-- Merging two sets of harness versions. A session sees several harness versions
-- because a laptop runs several concurrently, and the merge has to happen inside
-- the rollup upsert: reading the array out, merging in Go and writing it back
-- would lose one of two concurrent batches for the same session.
CREATE OR REPLACE FUNCTION session_array_union(a TEXT[], b TEXT[]) RETURNS TEXT[]
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
  SELECT CASE
    WHEN a IS NULL THEN b
    WHEN b IS NULL THEN a
    ELSE (SELECT array_agg(DISTINCT x ORDER BY x) FROM unnest(a || b) AS t(x))
  END
$$;

-- Adding two maps of counters. Used for the per-session redaction tallies, for
-- the same reason as session_array_union: the addition belongs in the statement
-- that already holds the row lock.
CREATE OR REPLACE FUNCTION jsonb_counter_add(a JSONB, b JSONB) RETURNS JSONB
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
  SELECT CASE
    WHEN a IS NULL OR jsonb_typeof(a) <> 'object' THEN b
    WHEN b IS NULL OR jsonb_typeof(b) <> 'object' THEN a
    ELSE (
      SELECT jsonb_object_agg(k, total)
      FROM (
        SELECT key AS k, sum(value::numeric) AS total
        FROM (
          SELECT key, value FROM jsonb_each_text(a)
          UNION ALL
          SELECT key, value FROM jsonb_each_text(b)
        ) merged
        GROUP BY key
      ) summed
    )
  END
$$;
