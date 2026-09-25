-- The skill catalog, and the reconciler runs that ground the compliance
-- report.
--
-- Three catalog tables and one ledger. skill_catalog_entries is one row per
-- skill per source repository (the marketplace, the backend mirror, the Devin
-- built-ins), keyed on (source_repo, plugin, skill) where skill is the mirror
-- directory name. The publisher's CI PUTs the whole catalog on every push to
-- main; an entry the body no longer carries flips present = false rather than
-- being deleted, so first_seen_at survives a rename and the pruning report can
-- count days in the catalog. skill_catalog_aliases is every name an
-- invocation row may use for an entry (the composite plugin:dir, the bare
-- dir, the frontmatter name): the alias join is how a skill_invocations row
-- resolves to an entry, and it is the oracle that confirms a typed command
-- the shape rule read rather than the transcript's own envelope (design 4a).
-- skill_catalog_publishes records each (source_repo, commit) once, which is
-- what makes a re-PUT of the same commit a duplicate rather than a second
-- write. reconciler_runs is one row per reconciler run (a Devin run date, a
-- Vorflux tick), unique on the platform and the run's own key rather than on
-- the token, so a run retried under a rotated token lands on the row the
-- compliance report and the platform summary read (design 3.7).
--
-- Foreign keys reach source_tokens alone. Nothing here references events,
-- sessions or messages, and no statement reads them: the catalog is written
-- by the publisher route and read by the reports, and a REFERENCES on the
-- largest table in the database would be the one statement in this file that
-- locked it inside the migration transaction.
--
-- Rollback floor: a revision from before this file neither reads nor writes
-- these four tables; it ignores them. Nothing has to be undone.

CREATE TABLE IF NOT EXISTS skill_catalog_entries (
  source_repo TEXT NOT NULL CHECK (source_repo ~ '^[a-z0-9][a-z0-9_.-]{0,31}$'), -- the short slug (for example skills, backend, vendor-builtin), the same regex the PUT checks on its path; never the owner/repo form, so path, body and the token's catalog-<source_repo> environment agree
  plugin TEXT NOT NULL DEFAULT '' CHECK (plugin = '' OR plugin ~ '^[a-z0-9][a-z0-9_-]{0,62}[a-z0-9]$'),
  skill TEXT NOT NULL CHECK (skill ~ '^[a-z0-9][a-z0-9_-]{0,62}[a-z0-9]$'),
  path TEXT NOT NULL DEFAULT '', sha256_tree TEXT NOT NULL DEFAULT '',
  installable BOOLEAN NOT NULL DEFAULT FALSE, mirrored BOOLEAN NOT NULL DEFAULT FALSE, skip_reason TEXT,
  authored_by TEXT NOT NULL DEFAULT 'unknown' CHECK (authored_by IN ('human', 'agent', 'vendor', 'unknown')),
  author_evidence TEXT NOT NULL DEFAULT '' CHECK (author_evidence IN ('', 'frontmatter', 'git_first_commit')),
  lineage_of TEXT, -- '<source_repo>/<plugin>:<skill>' of the lineage head; NULL on the head
  present BOOLEAN NOT NULL DEFAULT TRUE,
  first_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(), last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  commit TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (source_repo, plugin, skill));
CREATE TABLE IF NOT EXISTS skill_catalog_aliases (
  alias_plugin TEXT NOT NULL DEFAULT '', alias_skill TEXT NOT NULL, source_repo TEXT NOT NULL,
  plugin TEXT NOT NULL DEFAULT '', skill TEXT NOT NULL,
  PRIMARY KEY (alias_plugin, alias_skill, source_repo),
  FOREIGN KEY (source_repo, plugin, skill)
    REFERENCES skill_catalog_entries (source_repo, plugin, skill) ON DELETE CASCADE);
CREATE TABLE IF NOT EXISTS skill_catalog_publishes (
  source_repo TEXT NOT NULL, commit TEXT NOT NULL, generated_at TIMESTAMPTZ NOT NULL,
  received_at TIMESTAMPTZ NOT NULL DEFAULT now(), source_token_id UUID REFERENCES source_tokens(id),
  skills INTEGER NOT NULL, PRIMARY KEY (source_repo, commit));

-- The key CHECK is the ingest id shape; the UNIQUE is on the platform, not
-- the token, so a rotation never doubles a run (design 3.7).
CREATE TABLE IF NOT EXISTS reconciler_runs (
  id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  source_token_id UUID NOT NULL REFERENCES source_tokens(id),
  agent_platform TEXT NOT NULL CHECK (agent_platform IN ('devin', 'capy', 'codex', 'vorflux')),
  idempotency_key TEXT NOT NULL CHECK (idempotency_key ~ '^[0-9A-Za-z._:-]{1,64}$'),
  window_start TIMESTAMPTZ NOT NULL, window_end TIMESTAMPTZ NOT NULL,
  sessions_scanned INTEGER NOT NULL DEFAULT 0, sessions_with_events INTEGER NOT NULL DEFAULT 0,
  rows_posted INTEGER NOT NULL DEFAULT 0, truncated INTEGER NOT NULL DEFAULT 0,
  received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (agent_platform, idempotency_key));
