-- The vendor tables: sessions people ran on an agent platform that is not
-- ours, imported from a dump rather than captured by the loop-sessions
-- client. Applied by provision.sh through `bq query` alongside tables.sql,
-- with __PROJECT__ and __RAW_DATASET__ rendered; CREATE TABLE IF NOT
-- EXISTS, so it is safe to run after every edit.
--
-- Why these are separate tables and not more rows in sessions and
-- messages. The hourly export loads every partition it touches with
-- WRITE_TRUNCATE into a partition decorator (server/export/bigquery.go),
-- which replaces the whole day. A vendor row sharing a start day with a
-- captured session would be deleted by the next export of that day,
-- silently and at an hour nobody was watching. The export writes only the
-- five tables tables.sql declares and never sweeps the dataset, so a
-- separate pair of tables is untouchable by it. The union is a view
-- instead (views.sql, all_sessions and all_messages), which is where "one
-- uniform place" actually belongs: a view cannot be truncated, and it can
-- keep the two loading paths honest about which columns really mean the
-- same thing.
--
-- Column names match sessions and messages wherever the meaning matches,
-- so the union views are a plain SELECT and not a translation layer. What
-- has no counterpart keeps the vendor's own name (status_enum, playbook_id,
-- snapshot_id, structured_output).
--
-- platform is the vendor id, from the same five-value vocabulary as
-- skill_invocations.platform (claude_code, devin, capy, codex, vorflux).
-- The captured tables spell the same thing `source`; all_sessions reads one
-- from the other.
--
-- viewer_emails is the row-level access key the views in views.sql filter
-- on, exactly as for the captured tables. The importer does not invent it:
-- it takes the array the captured sessions already carry for the same
-- address, so the two paths cannot disagree about who a person is, and
-- falls back to the address alone. A session the dump gives no owner
-- (a sizeable share of a vendor dump is org automations) gets
-- an empty array, which is admins-only.
--
-- first_prompt and text are policy-tagged by provision.sh, as their
-- counterparts are; a dumped transcript is the same kind of text as a
-- captured one.

CREATE TABLE IF NOT EXISTS `__PROJECT__.__RAW_DATASET__.vendor_sessions` (
  session_id        STRING NOT NULL,
  platform          STRING NOT NULL,
  email             STRING,
  viewer_emails     ARRAY<STRING>,
  started_at        TIMESTAMP NOT NULL,
  ended_at          TIMESTAMP,
  status            STRING,
  status_enum       STRING,
  entrypoint        STRING,
  harness_title     STRING,
  first_prompt      STRING,
  tags              ARRAY<STRING>,
  playbook_id       STRING,
  snapshot_id       STRING,
  pull_request      JSON,
  structured_output JSON,
  user_turns        INT64,
  agent_turns       INT64,
  content_events    INT64,
  source_uri        STRING,
  imported_at       TIMESTAMP
)
PARTITION BY DATE(started_at)
CLUSTER BY email, platform
OPTIONS (description = 'One row per session run on a vendor agent platform, imported from a dump (examples/deploy-gcp/analytics/import-dv-sessions.sh). Not written by the hourly export. Query all_sessions.');

CREATE TABLE IF NOT EXISTS `__PROJECT__.__RAW_DATASET__.vendor_messages` (
  event_id           STRING NOT NULL,
  session_id         STRING NOT NULL,
  platform           STRING NOT NULL,
  email              STRING,
  viewer_emails      ARRAY<STRING>,
  session_started_at TIMESTAMP NOT NULL,
  seq                INT64,
  role               STRING,
  kind               STRING,
  occurred_at        TIMESTAMP,
  origin             STRING,
  username           STRING,
  user_id            STRING,
  text               STRING,
  imported_at        TIMESTAMP
)
PARTITION BY DATE(session_started_at)
CLUSTER BY email, session_id
OPTIONS (description = 'The text of a vendor session turn by turn, one row per message; text is policy-tagged. Query all_messages.');
