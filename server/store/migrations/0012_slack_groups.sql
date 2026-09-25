-- Named notification groups, and which sessions post to which.
--
-- A group is a destination with a name: "#eng-sessions", "my DM". Naming is
-- the whole feature — the env-variable era routed by raw channel id, which is
-- unmemorable, unshareable and silently stale. Visibility is two-valued:
-- 'org' means anybody may attach their OWN sessions to it (a shared team
-- channel), 'private' means the owner alone. Attaching somebody ELSE's
-- session is impossible at every surface by construction, because activating
-- a group posts the session owner's work and only their action can consent
-- to that.

CREATE TABLE IF NOT EXISTS slack_groups (
  id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  -- Unique per owner rather than globally: two people may both have "mine".
  -- Org-visible names still collide only per owner; the UI shows owner.
  name         TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 60),
  owner_email  TEXT NOT NULL REFERENCES principals(email) ON DELETE CASCADE,
  visibility   TEXT NOT NULL DEFAULT 'private' CHECK (visibility IN ('private', 'org')),
  -- 'dm' posts to the attaching session owner's DM (resolved per person at
  -- post time), a C…/G… id posts to that channel. The dm spelling means one
  -- org-visible "personal DM" group serves everybody.
  destination  TEXT NOT NULL CHECK (destination = 'dm' OR destination ~ '^[CG][A-Z0-9]{6,20}$'),
  disabled     BOOLEAN NOT NULL DEFAULT FALSE,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (owner_email, name)
);

-- Which sessions post to which groups: the activation, one row per pair.
-- Multi-group fan-out is a deliberate product decision — one session may
-- narrate into several destinations, each with its own thread and caps.
CREATE TABLE IF NOT EXISTS session_mirrors (
  session_id   TEXT NOT NULL REFERENCES sessions(session_id) ON DELETE CASCADE,
  group_id     BIGINT NOT NULL REFERENCES slack_groups(id) ON DELETE CASCADE,
  -- Who attached, always the session owner today; kept for the audit trail
  -- the moment admin tooling can detach.
  attached_by  TEXT NOT NULL,
  attached_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  detached_at  TIMESTAMPTZ,
  PRIMARY KEY (session_id, group_id)
);

CREATE INDEX IF NOT EXISTS session_mirrors_active_idx
  ON session_mirrors (attached_at DESC) WHERE detached_at IS NULL;

-- The per-person master kill for live mirroring, the counterpart of the
-- consent change: attaching a group is consent, and this one switch revokes
-- it everywhere at once without touching the standing digest preference.
ALTER TABLE slack_prefs ADD COLUMN IF NOT EXISTS live_disabled BOOLEAN NOT NULL DEFAULT FALSE;
-- The person's default group, auto-attached when a session arrives flagged
-- 'on' with no explicit group named.
ALTER TABLE slack_prefs ADD COLUMN IF NOT EXISTS default_group BIGINT REFERENCES slack_groups(id) ON DELETE SET NULL;
