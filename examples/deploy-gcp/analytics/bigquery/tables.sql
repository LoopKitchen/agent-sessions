-- The analytics tables the export job loads (server/export), in the raw
-- dataset (__RAW_DATASET__, loop_sessions_raw), whose access list names
-- the export service account and the admins group (provision.sh removes
-- the project groups BigQuery adds at creation; project-level BigQuery
-- roles still reach it: README, The access model). Nobody queries these
-- directly; the governed surface is the authorized views in views.sql,
-- which carry the access rule. Applied by provision.sh through `bq query`,
-- with __PROJECT__ and __RAW_DATASET__ rendered; idempotent, so it is safe
-- to run after every edit.
--
-- Every column here is a column of the matching projection in
-- server/store/export_reads.go, with the same name. The load jobs run with
-- ignoreUnknownValues off, so a column added there and not here fails the
-- load loudly (export_load_failed) until this file gains it; add it with
-- ALTER TABLE ... ADD COLUMN IF NOT EXISTS at the bottom, never by editing
-- a CREATE TABLE IF NOT EXISTS that will not run again.
--
-- Partitioning. turns, sessions and messages are partitioned by the
-- SESSION's start day (session_started_at, or started_at on sessions), not
-- the row's own time: the job rewrites a whole partition when any session
-- in it changes, and a session's turns and messages must therefore all be
-- in the partition its day names. events are partitioned by ingested_at,
-- which never changes for a stored row. health_hourly by the hour's day.
-- Timestamps are TIMESTAMP (UTC instants), which is what the export
-- renders (SET LOCAL TIME ZONE 'UTC', so every value carries +00:00).
--
-- viewer_emails is the row-level access key: the owner's address, its
-- Workspace alias and any principals row for the same person on those two
-- domains. The governed views in views.sql filter on it.
--
-- JSON columns (body, launcher, redactions, conditions) hold the Postgres
-- jsonb as BigQuery JSON; JSON cannot be a partition, cluster or row
-- policy column, which is why every key a policy or a partition needs is
-- a scalar beside it.

CREATE TABLE IF NOT EXISTS `__PROJECT__.__RAW_DATASET__.turns` (
  session_id           STRING NOT NULL,
  thread               STRING,
  turn_index           INT64,
  turn_key             STRING,
  email                STRING,
  viewer_emails        ARRAY<STRING>,
  source               STRING,
  session_started_at   TIMESTAMP NOT NULL,
  kind                 STRING,
  prompt_event_id      STRING,
  final_event_id       STRING,
  outcome              STRING,
  inherited            BOOL,
  started_at           TIMESTAMP,
  first_activity_at    TIMESTAMP,
  last_activity_at     TIMESTAMP,
  answered_at          TIMESTAMP,
  wall_ms              INT64,
  active_ms            INT64,
  idle_ms              INT64,
  waiting_for_human_ms INT64,
  origins              ARRAY<STRING>,
  merged               INT64,
  prompts              INT64,
  tool_calls           INT64,
  errors               INT64,
  subagents            INT64,
  files_changed        INT64,
  tokens_input         INT64,
  tokens_output        INT64,
  tokens_cache_read    INT64,
  tokens_cache_write   INT64,
  cost_usd             NUMERIC,
  model                STRING,
  derived_version      INT64,
  derived_at           TIMESTAMP,
  exported_at          TIMESTAMP
)
PARTITION BY DATE(session_started_at)
CLUSTER BY email, session_id
OPTIONS (description = 'One row per logical exchange (loop-sessions turns), rewritten whole per session start day by the hourly export');

CREATE TABLE IF NOT EXISTS `__PROJECT__.__RAW_DATASET__.sessions` (
  session_id         STRING NOT NULL,
  email              STRING,
  viewer_emails      ARRAY<STRING>,
  device_id          STRING,
  source             STRING,
  parent_session_id  STRING,
  lineage_source     STRING,
  parent_record_uuid STRING,
  cwd                STRING,
  repo               STRING,
  git_branch         STRING,
  started_at         TIMESTAMP NOT NULL,
  ended_at           TIMESTAMP,
  ended              BOOL,
  session_type       STRING,
  empty_kind         STRING,
  head_state         STRING,
  entrypoint         STRING,
  launcher           JSON,
  transcript_exists  BOOL,
  user_turns         INT64,
  human_turns        INT64,
  tool_calls         INT64,
  subagents          INT64,
  errors             INT64,
  content_events     INT64,
  first_prompt       STRING,
  title_source       STRING,
  harness_title      STRING,
  harness_versions   ARRAY<STRING>,
  agent_versions     ARRAY<STRING>,
  tokens_input       INT64,
  tokens_output      INT64,
  tokens_cache_read  INT64,
  tokens_cache_write INT64,
  cost_usd           NUMERIC,
  redactions         JSON,
  ingested_at        TIMESTAMP,
  updated_at         TIMESTAMP,
  exported_at        TIMESTAMP
)
PARTITION BY DATE(started_at)
CLUSTER BY email, session_type
OPTIONS (description = 'One row per session (the Postgres sessions rollup), rewritten whole per start day by the hourly export');

CREATE TABLE IF NOT EXISTS `__PROJECT__.__RAW_DATASET__.messages` (
  event_id           STRING NOT NULL,
  session_id         STRING NOT NULL,
  email              STRING,
  viewer_emails      ARRAY<STRING>,
  session_started_at TIMESTAMP NOT NULL,
  seq                INT64,
  role               STRING,
  kind               STRING,
  agent_id           STRING,
  occurred_at        TIMESTAMP,
  origin             STRING,
  superseded_by      STRING,
  text               STRING,
  exported_at        TIMESTAMP
)
PARTITION BY DATE(session_started_at)
CLUSTER BY email, session_id
OPTIONS (description = 'The text of prompts, answers and tool results, one row per text-bearing event, classified by kind; text is policy-tagged');

CREATE TABLE IF NOT EXISTS `__PROJECT__.__RAW_DATASET__.events` (
  id                 STRING NOT NULL,
  session_id         STRING NOT NULL,
  email              STRING,
  viewer_emails      ARRAY<STRING>,
  seq                INT64,
  type               STRING,
  origin             STRING,
  occurred_at        TIMESTAMP,
  ingested_at        TIMESTAMP NOT NULL,
  agent_id           STRING,
  workflow_id        STRING,
  model              STRING,
  tool_name          STRING,
  capture_version    INT64,
  prompt_id          STRING,
  record_uuid        STRING,
  parent_record_uuid STRING,
  tool_use_id        STRING,
  message_id         STRING,
  request_id         STRING,
  superseded_by      STRING,
  body_expired       BOOL,
  body               JSON,
  exported_at        TIMESTAMP
)
PARTITION BY DATE(ingested_at)
CLUSTER BY session_id, type
OPTIONS (description = 'Every captured event with its derived body (the raw transcript line is not exported); body is policy-tagged. Query events_latest.');

CREATE TABLE IF NOT EXISTS `__PROJECT__.__RAW_DATASET__.health_hourly` (
  email                STRING,
  viewer_emails        ARRAY<STRING>,
  device_id            STRING,
  hour                 TIMESTAMP NOT NULL,
  reports              INT64,
  worst                STRING,
  conditions           JSON,
  drops                INT64,
  quarantined          INT64,
  parked               INT64,
  capture_blocked_secs INT64,
  empty_starts         INT64,
  exported_at          TIMESTAMP
)
PARTITION BY DATE(hour)
CLUSTER BY email
OPTIONS (description = 'Per-device hourly fleet health rollup (loop-sessions health_hourly)');
