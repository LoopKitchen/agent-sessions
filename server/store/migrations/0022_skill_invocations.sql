-- Skill invocations, and the source tokens that emit them from platforms
-- that are not an enrolled laptop.
--
-- One row per time a skill was invoked on any agent platform, whichever way
-- the fact reached us. `origin` says how: derived rows come from events this
-- store already holds (a Skill tool_call, or a typed slash command on a
-- user_prompt) and are written in the same transaction as the events
-- (server/store/store.go UpsertEvents, after insertLinks) and re-derived by
-- the runner's skill_invocations step; hook, beacon and reconciler rows arrive
-- through POST /v1/skill-invocations and are the only record of their
-- invocation. Every copy of one moment collapses to one row through
-- dedupe_key (see the unique index), and a device-verified copy always wins
-- over a claimed one.
--
-- No foreign key to sessions or events, on purpose. A Devin or Capy session
-- id is not in sessions, and a Claude Code session past its retention window
-- must not take its skill history with it: the pruning policy needs ninety
-- days of usage whatever RETENTION_SESSION_DAYS is set to. event_id is plain
-- text for the same reason, and because a REFERENCES events(id) would be the
-- one statement in this file that locks the largest table in the database
-- inside the migration transaction. Nothing here is free text: args are
-- never stored, cwd is reduced to its last segment, the vendor session id is
-- bounded by the same shape as an event id.
--
-- Rollback floor: a revision from before this file neither reads nor writes
-- either table; it ignores them. Nothing has to be undone.

-- A source token proves a platform, not a person: a Devin organisation, a
-- Capy project, a Codex or Claude Code cloud environment, a Vorflux harness,
-- or the skills repository's CI publisher. Every row written under one is
-- trust = 'claimed'. One row per (platform, environment) in normal operation;
-- two live rows during a rotation. Only the sha256 is stored, as for
-- device_tokens; the partial unique index mirrors device_tokens_token_hash_idx.
-- The token prefix is lss_ (four characters, like lsd_), so /v1/events refuses
-- a source token by prefix before any lookup and a scanner can tell them apart.
CREATE TABLE IF NOT EXISTS source_tokens (
  id             UUID PRIMARY KEY,
  platform       TEXT NOT NULL CHECK (platform IN ('claude_code', 'devin', 'capy', 'codex', 'vorflux')),
  -- 'default', 'cloud', a Capy project slug, a Codex environment name.
  environment    TEXT NOT NULL DEFAULT 'default' CHECK (environment ~ '^[a-z0-9][a-z0-9_.-]{0,39}$'),
  -- One scope per token. skill-catalog is the CI publisher's (R5 2.4 option A).
  scope          TEXT NOT NULL DEFAULT 'skill-invocations' CHECK (scope IN ('skill-invocations', 'skill-catalog')),
  label          TEXT CHECK (label IS NULL OR length(label) <= 80),
  token_hash     BYTEA NOT NULL,
  issued_by      TEXT NOT NULL REFERENCES principals(email),
  issued_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at     TIMESTAMPTZ,
  revoked_at     TIMESTAMPTZ,
  revoked_by     TEXT REFERENCES principals(email),
  last_used_at   TIMESTAMPTZ,
  -- Rate accounting on the row, so the bound is one statement with the
  -- verification and is shared by every Cloud Run instance without a Redis
  -- this service does not otherwise run (server/app/enroll.go says why).
  window_started TIMESTAMPTZ,
  -- DEV-a (design 3.1): the per-token rate limit is a row value an admin edits, not a constant.
  rate_limit_per_min INTEGER NOT NULL DEFAULT 1200 CHECK (rate_limit_per_min BETWEEN 0 AND 100000),
  -- DEV-d (design 3.1): the origins a token may post are bound to the credential at mint, never a payload claim.
  allowed_origins TEXT[] NOT NULL DEFAULT '{beacon}' CHECK (cardinality(allowed_origins) > 0 AND allowed_origins <@ ARRAY['hook', 'beacon', 'reconciler']),
  -- DEV-e (design 3.1): a per-person laptop token names its holder here; every shared token leaves it NULL.
  bound_actor_email TEXT,
  window_count   INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS source_tokens_token_hash_idx
  ON source_tokens (token_hash) WHERE revoked_at IS NULL;
CREATE INDEX IF NOT EXISTS source_tokens_platform_idx
  ON source_tokens (platform, environment) WHERE revoked_at IS NULL;

CREATE TABLE IF NOT EXISTS skill_invocations (
  id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  -- When the skill ran as the emitter saw it; received_at is when we stored
  -- it. A claimed occurred_at outside [now-7d, now+5m] is replaced by
  -- received_at and time_clamped is set, so a leaked token can inflate the
  -- present but never rewrite last quarter.
  occurred_at      TIMESTAMPTZ NOT NULL,
  received_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  time_clamped     BOOLEAN NOT NULL DEFAULT FALSE,
  -- derived: from this store's own events. hook: a Claude Code or Codex hook
  -- posting live. beacon: a curl inside a SKILL.md step. reconciler: a job
  -- replaying a vendor's own activation log.
  origin           TEXT NOT NULL CHECK (origin IN ('derived', 'hook', 'beacon', 'reconciler')),
  agent_platform   TEXT NOT NULL CHECK (agent_platform IN ('claude_code', 'devin', 'capy', 'codex', 'vorflux')),
  -- Which credential vouched. device: the row came through a device token
  -- (derived rows, or an lsd_ hook), so actor_email is proven. claimed: a
  -- source token proved the platform and the person is whatever the payload
  -- said. A device row can never carry a source token.
  trust            TEXT NOT NULL CHECK (trust IN ('device', 'claimed')),
  device_id        UUID,
  source_token_id  UUID REFERENCES source_tokens(id),
  -- DEV-f (design 3.1): the claimed token a device-verified copy displaced on this key.
  preempted_by UUID REFERENCES source_tokens(id),
  -- DEV-h (design 3.1): the lsd_ device a derived copy displaced, since such a row has no source token to name.
  preempted_device UUID,
  CHECK (trust = 'claimed' OR source_token_id IS NULL),
  -- What the emitter called the skill, bounded and control-stripped, never
  -- rejected: an unresolvable name is the "unknown skill" bucket, which is a
  -- report (R5 R14), not an error.
  raw_name         TEXT NOT NULL CHECK (length(raw_name) BETWEEN 1 AND 200),
  -- The server's normalisation of raw_name: strip a leading '/', split on the
  -- last ':', lowercase. plugin is '' for a bare name; skill is NULL when the
  -- result does not fit the bare-slug shape. Catalog resolution joins on
  -- (plugin, skill) through the catalog's aliases, never on raw_name.
  plugin           TEXT NOT NULL DEFAULT '' CHECK (plugin = '' OR plugin ~ '^[a-z0-9][a-z0-9_-]{0,62}[a-z0-9]$'),
  skill            TEXT CHECK (skill IS NULL OR skill ~ '^[a-z0-9][a-z0-9_-]{0,62}[a-z0-9]$'),
  -- plugin (marketplace), project (.claude/skills), user (~/.claude/skills),
  -- mirror (.agents/skills), builtin (a vendor's own), unknown.
  skill_source     TEXT NOT NULL DEFAULT 'unknown' CHECK (skill_source IN ('plugin', 'project', 'user', 'mirror', 'builtin', 'unknown')),
  -- user: a typed slash command (UserPromptExpansion, or a slash_command
  -- prompt). agent: the model called the Skill tool. nested and preload are
  -- the harness's other two triggers when an emitter can tell; unknown
  -- otherwise. A Skill tool_call row is never 'user' (R2 2.3).
  trigger          TEXT NOT NULL DEFAULT 'unknown' CHECK (trigger IN ('user', 'agent', 'nested', 'preload', 'unknown')),
  -- started: PreToolUse or a user_prompt proves intent, not completion. A
  -- later copy with the same dedupe_key (PostToolUse, tool_failed) updates it.
  outcome          TEXT NOT NULL DEFAULT 'unknown' CHECK (outcome IN ('started', 'success', 'error', 'unknown')),
  error_class      TEXT CHECK (error_class IN ('not_found', 'timeout', 'runtime_error', 'unknown')),
  CHECK (error_class IS NULL OR outcome = 'error'),
  -- Proven for device rows (events.email); the payload's claim otherwise,
  -- accepted only when it is one address in an allowed domain, with actor_known
  -- set when it matched a live principals row at ingest. NULL for automation
  -- sessions with no person.
  actor_email      TEXT,
  actor_known      BOOLEAN NOT NULL DEFAULT FALSE,
  -- Last path segment only (ingest.RepoFromCwd); never a path.
  repo             TEXT NOT NULL DEFAULT '' CHECK (repo = '' OR repo ~ '^[0-9A-Za-z._-]{1,64}$'),
  -- The harness's session id: sessions.session_id for Claude Code and Codex
  -- rows, the vendor's id for Devin, Capy and Vorflux rows, '' when the
  -- platform exposes none to the emitter. Free of any foreign key.
  session_ref      TEXT NOT NULL DEFAULT '' CHECK (session_ref = '' OR session_ref ~ '^[0-9A-Za-z._:-]{1,64}$'),
  -- DEV-b (design 3.1): the reconciler's lsref tag, joined to a beacon row's session_ref.
  link_ref TEXT CHECK (link_ref IS NULL OR link_ref ~ '^[0-9A-Za-z._:-]{1,64}$'),
  -- Copied from sessions.session_type by the derive step, because the row
  -- outlives the session; the dashboard hides internal and automation rows
  -- by default.
  -- DEV-c (design 3.1): the closed set is the sessions CHECK plus '' for not yet copied.
  session_type TEXT NOT NULL DEFAULT '' CHECK (session_type IN ('', 'empty', 'user', 'internal', 'automation')),
  -- Derived rows only: the events row this came from (plain text, no FK).
  event_id         TEXT,
  -- The harness ids that pair copies of one moment. Both NULL on a beacon
  -- row from a platform with no ids.
  prompt_id        TEXT,
  tool_use_id      TEXT,
  -- The emitter's own replay key, when it sent one (beacons, reconcilers).
  idempotency_key  TEXT CHECK (idempotency_key IS NULL OR idempotency_key ~ '^[0-9A-Za-z._:-]{1,64}$'),
  -- Computed by the server, never by the client; see the handler for the
  -- composition. One unique index over it is the whole dedupe story.
  dedupe_key       TEXT NOT NULL CHECK (length(dedupe_key) BETWEEN 1 AND 200),
  -- Only whether arguments were passed and how long they were. The text may
  -- carry customer identifiers and is never stored (U10).
  args_present     BOOLEAN NOT NULL DEFAULT FALSE,
  args_bytes       INTEGER NOT NULL DEFAULT 0,
  harness_version  TEXT NOT NULL DEFAULT '' CHECK (harness_version = '' OR harness_version ~ '^[0-9A-Za-z._-]{1,40}$')
);

-- The dedupe key. Every copy of one invocation lands on the same key and
-- the insert is ON CONFLICT (dedupe_key) DO UPDATE with the merge rule in
-- the handler (device beats claimed; success or error beats started).
CREATE UNIQUE INDEX IF NOT EXISTS skill_invocations_dedupe_idx
  ON skill_invocations (dedupe_key);
-- The /skills page: per skill, per person, per platform, each over a range.
CREATE INDEX IF NOT EXISTS skill_invocations_skill_occurred_idx
  ON skill_invocations (plugin, skill, occurred_at DESC);
CREATE INDEX IF NOT EXISTS skill_invocations_actor_occurred_idx
  ON skill_invocations (actor_email, occurred_at DESC);
CREATE INDEX IF NOT EXISTS skill_invocations_platform_occurred_idx
  ON skill_invocations (agent_platform, occurred_at DESC);
-- The derive step's per-session upsert and the session page's skill list.
CREATE INDEX IF NOT EXISTS skill_invocations_session_idx
  ON skill_invocations (session_ref) WHERE origin = 'derived';
-- DEV-b (design 3.1): the reconciler-to-beacon join reads link_ref.
CREATE INDEX IF NOT EXISTS skill_invocations_link_ref_idx ON skill_invocations (link_ref) WHERE link_ref IS NOT NULL;

-- DEV-g (design 3.1): sessions whose skill rows a savepoint rollback, a lock wait, a step skip or a rollback window lost; drained by the dirty tick, parked at five attempts.
CREATE TABLE IF NOT EXISTS skill_rederive_queue (session_id TEXT PRIMARY KEY, reason TEXT NOT NULL, queued_at TIMESTAMPTZ NOT NULL DEFAULT now(), attempts INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT '');

-- DEV-i (design 3.1): the audit row behind every admin mutation, written inside the caller's transaction.
CREATE TABLE IF NOT EXISTS admin_actions (id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY, actor TEXT NOT NULL, action TEXT NOT NULL, target TEXT NOT NULL DEFAULT '', detail JSONB NOT NULL DEFAULT '{}', at TIMESTAMPTZ NOT NULL DEFAULT now());
