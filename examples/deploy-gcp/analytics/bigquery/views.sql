-- The governed surface: every table analysts query is a view in the
-- __DATASET__ dataset (loop_sessions) over the raw __RAW_DATASET__
-- dataset (loop_sessions_raw) the export job loads into. CREATE OR REPLACE,
-- so provision.sh can apply this file as often as it likes; run it after a
-- column is added to a raw table, because a view's column list is fixed
-- when it is created.
--
-- Why views and not row access policies on the loaded tables: every load
-- the job issues is WRITE_TRUNCATE into a partition decorator, and BigQuery
-- documents that WRITE_TRUNCATE operations remove all row access policies
-- and all policy tags on the destination table (row-level-security-intro,
-- column-level-security-intro). Whether the
-- partition-decorator form is exempt is not documented; the one scratch
-- experiment recorded in tasks/F/review-1.md ("Lead experiment") saw a
-- row access policy survive such a load, which is an observation, not a
-- guarantee. A view is never truncated: the access rule lives in its SQL,
-- the raw dataset's access list names the export service account and the
-- admins group (provision.sh removes the project groups BigQuery adds at
-- creation; project-level BigQuery roles still reach it: README, The
-- access model), and nothing the hourly job does can widen who sees what.
--
-- The rule, on every view: a row is visible to the people it names in
-- viewer_emails (the owner and its Workspace alias; SESSION_USER() is the
-- querying identity) and to the admins listed in __RAW_DATASET__.admins
-- (admins.sql; provision.sh fills it from --admin-emails). BigQuery has no
-- group-membership function, which is why the admins are a table and not
-- the admins group. A principal the rule does not admit gets zero rows,
-- never an error.
--
-- These are authorized views: provision.sh adds each of them to the raw
-- dataset's access list (a "view" entry), so the view reads the raw tables
-- on the querying user's behalf without the user holding any access to
-- __RAW_DATASET__. Column-level security still applies to the user:
-- reading messages.text or events.body through a view needs Fine-Grained
-- Reader on the pii_text tag (provision.sh, column tags).
--
-- Partition pruning passes through the five plain views: filter on the
-- same columns as on the raw tables (session_started_at on turns and
-- messages, started_at on sessions, ingested_at on events, hour on
-- health_hourly). It does not pass through the four *_latest views below:
-- there the caller's date filter sits above a ROW_NUMBER() window keyed on
-- the row's identity, not on the partition column, and pushing it under
-- the window would change the answer in exactly the moved-day case the
-- views exist for; so a query on a *_latest view reads every partition the
-- caller can see. Small today; for a big date-bounded scan use the plain
-- view and dedupe by hand (README, How to query).

CREATE OR REPLACE VIEW `__PROJECT__.__DATASET__.turns` AS
SELECT * FROM `__PROJECT__.__RAW_DATASET__.turns`
WHERE SESSION_USER() IN UNNEST(viewer_emails)
   OR SESSION_USER() IN (SELECT email FROM `__PROJECT__.__RAW_DATASET__.admins`);

CREATE OR REPLACE VIEW `__PROJECT__.__DATASET__.sessions` AS
SELECT * FROM `__PROJECT__.__RAW_DATASET__.sessions`
WHERE SESSION_USER() IN UNNEST(viewer_emails)
   OR SESSION_USER() IN (SELECT email FROM `__PROJECT__.__RAW_DATASET__.admins`);

CREATE OR REPLACE VIEW `__PROJECT__.__DATASET__.messages` AS
SELECT * FROM `__PROJECT__.__RAW_DATASET__.messages`
WHERE SESSION_USER() IN UNNEST(viewer_emails)
   OR SESSION_USER() IN (SELECT email FROM `__PROJECT__.__RAW_DATASET__.admins`);

CREATE OR REPLACE VIEW `__PROJECT__.__DATASET__.events` AS
SELECT * FROM `__PROJECT__.__RAW_DATASET__.events`
WHERE SESSION_USER() IN UNNEST(viewer_emails)
   OR SESSION_USER() IN (SELECT email FROM `__PROJECT__.__RAW_DATASET__.admins`);

CREATE OR REPLACE VIEW `__PROJECT__.__DATASET__.health_hourly` AS
SELECT * FROM `__PROJECT__.__RAW_DATASET__.health_hourly`
WHERE SESSION_USER() IN UNNEST(viewer_emails)
   OR SESSION_USER() IN (SELECT email FROM `__PROJECT__.__RAW_DATASET__.admins`);

-- The *_latest views: one row per key, the most recently exported copy.
--
-- events_latest: a stored event keeps its id and its ingested_at for life,
-- so a day partition rewritten whole already holds exactly one copy of
-- each; the view is the belt to that brace, keyed on (id, capture_version)
-- as contract F names it, so a re-walk that raised a row's capture_version
-- reads as the one row it is even if two copies were ever loaded.
-- exported_at breaks the tie between two copies at the same version.
--
-- sessions_latest, turns_latest, messages_latest: a session's started_at
-- can move earlier when a batch with an older event arrives (the rollup
-- takes the least), which moves the session, and with it its turns and
-- messages, to an earlier day. The export rewrites the new day's
-- partitions and cannot know the old day held a copy, so the old day keeps
-- one until it is rewritten for another reason (review-1 M2). These views
-- hide the old copy for all three session-keyed tables; the raw partitions
-- keep it, and a full re-export (README, Runbook: export) clears it.

CREATE OR REPLACE VIEW `__PROJECT__.__DATASET__.events_latest` AS
SELECT * EXCEPT (rn)
FROM (
  SELECT *,
         ROW_NUMBER() OVER (PARTITION BY id ORDER BY capture_version DESC, exported_at DESC) AS rn
  FROM `__PROJECT__.__RAW_DATASET__.events`
  WHERE SESSION_USER() IN UNNEST(viewer_emails)
     OR SESSION_USER() IN (SELECT email FROM `__PROJECT__.__RAW_DATASET__.admins`)
)
WHERE rn = 1;

CREATE OR REPLACE VIEW `__PROJECT__.__DATASET__.sessions_latest` AS
SELECT * EXCEPT (rn)
FROM (
  SELECT *,
         ROW_NUMBER() OVER (PARTITION BY session_id ORDER BY exported_at DESC) AS rn
  FROM `__PROJECT__.__RAW_DATASET__.sessions`
  WHERE SESSION_USER() IN UNNEST(viewer_emails)
     OR SESSION_USER() IN (SELECT email FROM `__PROJECT__.__RAW_DATASET__.admins`)
)
WHERE rn = 1;

CREATE OR REPLACE VIEW `__PROJECT__.__DATASET__.turns_latest` AS
SELECT * EXCEPT (rn)
FROM (
  SELECT *,
         ROW_NUMBER() OVER (PARTITION BY session_id, thread, turn_index ORDER BY exported_at DESC) AS rn
  FROM `__PROJECT__.__RAW_DATASET__.turns`
  WHERE SESSION_USER() IN UNNEST(viewer_emails)
     OR SESSION_USER() IN (SELECT email FROM `__PROJECT__.__RAW_DATASET__.admins`)
)
WHERE rn = 1;

CREATE OR REPLACE VIEW `__PROJECT__.__DATASET__.messages_latest` AS
SELECT * EXCEPT (rn)
FROM (
  SELECT *,
         ROW_NUMBER() OVER (PARTITION BY event_id ORDER BY exported_at DESC) AS rn
  FROM `__PROJECT__.__RAW_DATASET__.messages`
  WHERE SESSION_USER() IN UNNEST(viewer_emails)
     OR SESSION_USER() IN (SELECT email FROM `__PROJECT__.__RAW_DATASET__.admins`)
)
WHERE rn = 1;

-- The vendor views, and the union.
--
-- vendor_sessions and vendor_messages (bigquery/vendor.sql) hold sessions
-- run on an agent platform that is not ours, imported from a dump rather
-- than captured by the client. They carry the same viewer_emails key, so
-- they take the same rule, and they are authorized on the raw dataset by
-- the same loop in provision.sh, which reads this file for its list.

CREATE OR REPLACE VIEW `__PROJECT__.__DATASET__.vendor_sessions` AS
SELECT * FROM `__PROJECT__.__RAW_DATASET__.vendor_sessions`
WHERE SESSION_USER() IN UNNEST(viewer_emails)
   OR SESSION_USER() IN (SELECT email FROM `__PROJECT__.__RAW_DATASET__.admins`);

CREATE OR REPLACE VIEW `__PROJECT__.__DATASET__.vendor_messages` AS
SELECT * FROM `__PROJECT__.__RAW_DATASET__.vendor_messages`
WHERE SESSION_USER() IN UNNEST(viewer_emails)
   OR SESSION_USER() IN (SELECT email FROM `__PROJECT__.__RAW_DATASET__.admins`);

-- all_sessions and all_messages are the one uniform place: every session a
-- person ran, whichever platform ran it, under one access rule and one set
-- of column names.
--
-- They select from the governed views above rather than from the raw
-- tables, so the rule is written once; SESSION_USER() is the querying
-- identity however deeply a view is nested, and the inner views are the
-- ones authorized on the raw dataset. The captured side reads the *_latest
-- views so a session whose start day moved is not counted twice.
--
-- Only the columns that mean the same thing on both sides are here. The
-- captured tables spell the platform `source` and the vendor tables spell
-- it `platform`; everything else that survives the union keeps its name.
-- `ingest` says which path a row came in by, because the two are not
-- equally complete: a captured session has turns, tokens, cost and tool
-- calls, and an imported one has whatever the dump held. For those, query
-- sessions_latest or vendor_sessions directly.
--
-- all_messages joins the captured side back to sessions_latest for the
-- platform, because messages carries no source of its own and the captured
-- corpus is not one platform: about one session in ten in a real corpus
-- is codex, not claude_code, so a literal there would have been wrong for
-- one session in ten. The join is a LEFT JOIN so a message whose
-- session is outside the reader's rows still appears with a null platform
-- rather than vanishing.

CREATE OR REPLACE VIEW `__PROJECT__.__DATASET__.all_sessions` AS
SELECT
  'captured'        AS ingest,
  source            AS platform,
  session_id,
  email,
  started_at,
  ended_at,
  entrypoint,
  harness_title,
  first_prompt,
  user_turns,
  content_events
FROM `__PROJECT__.__DATASET__.sessions_latest`
UNION ALL
SELECT
  'imported'        AS ingest,
  platform,
  session_id,
  email,
  started_at,
  ended_at,
  entrypoint,
  harness_title,
  first_prompt,
  user_turns,
  content_events
FROM `__PROJECT__.__DATASET__.vendor_sessions`;

CREATE OR REPLACE VIEW `__PROJECT__.__DATASET__.all_messages` AS
SELECT
  'captured'        AS ingest,
  s.source          AS platform,
  m.event_id,
  m.session_id,
  m.email,
  m.session_started_at,
  m.seq,
  m.role,
  m.kind,
  m.occurred_at,
  m.origin,
  m.text
FROM `__PROJECT__.__DATASET__.messages_latest` m
LEFT JOIN `__PROJECT__.__DATASET__.sessions_latest` s USING (session_id)
UNION ALL
SELECT
  'imported'        AS ingest,
  platform,
  event_id,
  session_id,
  email,
  session_started_at,
  seq,
  role,
  kind,
  occurred_at,
  origin,
  text
FROM `__PROJECT__.__DATASET__.vendor_messages`;
