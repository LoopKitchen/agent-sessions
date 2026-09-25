-- Turns one vendor dump, already loaded into the two staging tables, into
-- the rows of vendor_sessions and vendor_messages. Rendered and run by
-- import-dv-sessions.sh with __PROJECT__, __RAW_DATASET__, __PLATFORM__,
-- __STAGE_DUMP__, __STAGE_INDEX__ and __DUMP_URI__ filled in.
--
-- It is one transaction, and it deletes this platform's rows before
-- inserting them, so re-running it against a corrected dump replaces what
-- the last run wrote and never doubles it. Another platform's rows are not
-- touched: the DELETE is keyed on platform, not on the table.
--
-- Identity. The dump's per-session files carry no owner; the index does,
-- as requesting_user_email, and a sizeable share of the sessions in a
-- vendor dump have none at all (org automations). viewer_emails
-- is not invented here: it is the union of every viewer_emails the
-- captured sessions already carry for the same address, which is the
-- mapping the ingest path computed from the principals table, plus the
-- address itself for someone the captured corpus has never seen
-- (a vendor-side automation account that ran hundreds of these and has
-- no loop-sessions row). A
-- session with no owner gets an empty array, which the views read as
-- admins-only. Guessing an alias from the local part is what this avoids:
-- two people with the same local part on two domains are not the same
-- person, and nothing in the dump says they are.

BEGIN TRANSACTION;

DELETE FROM `__PROJECT__.__RAW_DATASET__.vendor_sessions` WHERE platform = '__PLATFORM__';
DELETE FROM `__PROJECT__.__RAW_DATASET__.vendor_messages` WHERE platform = '__PLATFORM__';

INSERT INTO `__PROJECT__.__RAW_DATASET__.vendor_sessions` (
  session_id, platform, email, viewer_emails, started_at, ended_at, status, status_enum,
  entrypoint, harness_title, first_prompt, tags, playbook_id, snapshot_id,
  pull_request, structured_output, user_turns, agent_turns, content_events,
  source_uri, imported_at)
WITH aliases AS (
  SELECT email, ARRAY_AGG(DISTINCT v) AS viewer_emails
  FROM `__PROJECT__.__RAW_DATASET__.sessions`, UNNEST(viewer_emails) AS v
  WHERE email IS NOT NULL
  GROUP BY email
)
SELECT
  d.session_id,
  '__PLATFORM__',
  i.requesting_user_email,
  ARRAY(SELECT DISTINCT x
        FROM UNNEST(ARRAY_CONCAT(COALESCE(a.viewer_emails, []),
                                 [i.requesting_user_email])) AS x
        WHERE x IS NOT NULL),
  d.created_at,
  d.updated_at,
  d.status,
  d.status_enum,
  (SELECT JSON_VALUE(m, '$.origin') FROM UNNEST(JSON_QUERY_ARRAY(d.messages)) AS m WITH OFFSET o
    WHERE JSON_VALUE(m, '$.type') = 'initial_user_message' ORDER BY o LIMIT 1),
  d.title,
  (SELECT JSON_VALUE(m, '$.message') FROM UNNEST(JSON_QUERY_ARRAY(d.messages)) AS m WITH OFFSET o
    WHERE JSON_VALUE(m, '$.type') = 'initial_user_message' ORDER BY o LIMIT 1),
  d.tags,
  d.playbook_id,
  d.snapshot_id,
  d.pull_request,
  d.structured_output,
  (SELECT COUNT(*) FROM UNNEST(JSON_QUERY_ARRAY(d.messages)) AS m
    WHERE JSON_VALUE(m, '$.type') IN ('user_message', 'initial_user_message')),
  (SELECT COUNT(*) FROM UNNEST(JSON_QUERY_ARRAY(d.messages)) AS m
    WHERE JSON_VALUE(m, '$.type') = 'devin_message'),
  ARRAY_LENGTH(JSON_QUERY_ARRAY(d.messages)),
  '__DUMP_URI__/sessions/' || d.session_id || '.json',
  CURRENT_TIMESTAMP()
FROM `__PROJECT__.__RAW_DATASET__.__STAGE_DUMP__` AS d
LEFT JOIN `__PROJECT__.__RAW_DATASET__.__STAGE_INDEX__` AS i USING (session_id)
LEFT JOIN aliases AS a ON a.email = i.requesting_user_email
WHERE d.created_at IS NOT NULL;

INSERT INTO `__PROJECT__.__RAW_DATASET__.vendor_messages` (
  event_id, session_id, platform, email, viewer_emails, session_started_at,
  seq, role, kind, occurred_at, origin, username, user_id, text, imported_at)
SELECT
  JSON_VALUE(m, '$.event_id'),
  s.session_id,
  s.platform,
  s.email,
  s.viewer_emails,
  s.started_at,
  o,
  IF(JSON_VALUE(m, '$.type') = 'devin_message', 'assistant', 'user'),
  JSON_VALUE(m, '$.type'),
  SAFE_CAST(JSON_VALUE(m, '$.timestamp') AS TIMESTAMP),
  JSON_VALUE(m, '$.origin'),
  JSON_VALUE(m, '$.username'),
  JSON_VALUE(m, '$.user_id'),
  JSON_VALUE(m, '$.message'),
  CURRENT_TIMESTAMP()
FROM `__PROJECT__.__RAW_DATASET__.vendor_sessions` AS s
JOIN `__PROJECT__.__RAW_DATASET__.__STAGE_DUMP__` AS d USING (session_id),
UNNEST(JSON_QUERY_ARRAY(d.messages)) AS m WITH OFFSET o
WHERE s.platform = '__PLATFORM__'
  AND JSON_VALUE(m, '$.event_id') IS NOT NULL;

COMMIT TRANSACTION;
