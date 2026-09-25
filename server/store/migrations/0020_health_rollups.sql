-- Health rollups: the newest report per machine, one row per machine per
-- hour, and the mute table the fleet evaluator honours.
--
-- health_reports grows by a row per machine every five minutes (about 20k
-- rows and 7-22 MB a day at 26 devices, research/r6 F9) and every fleet read
-- scanned it for the newest row per device. Two derived tables end that:
--
-- health_latest holds the newest report per (email, device_id), upserted by
-- PutHealthReport in the same transaction as the raw insert, so the fleet
-- page, the coverage report and the evaluator read 26 rows instead of a
-- DISTINCT ON over the whole history. It outlives the raw rows: a machine
-- that last reported eight days ago is still on this table with what it last
-- said, which is exactly the machine the fleet page exists to show.
--
-- health_hourly is the ledger the alerts and the capture-loss rule read: per
-- machine per hour, how many reports arrived, the worst level, the condition
-- census, and the counters that only mean something as a delta (dropped
-- events, quarantined and parked items, seconds spent capture-blocked, the
-- empty starts the client counted). It is recomputed from the raw rows for
-- any hour, so the raw rows can be deleted after seven days without losing
-- the fleet's history; the sweep in server/store/health.go does both.
--
-- health_reports_received_at_idx is what the seven-day sweep deletes by.
-- The table holds a few hundred thousand rows, so the build is a second or
-- two of SHARE lock against health posts, which retry; measured cost is well
-- inside the migration budget and the alternative, a concurrent build by the
-- runner, would leave the sweep scanning the table until the runner's window.
--
-- fleet_mutes silences one (person, kind) until a moment, with a note that
-- says why: the empty-start rule names the same five people every day until
-- their scripts change, and an alert that fires daily about a known cause is
-- an alert everybody learns to ignore. The evaluator skips a muted pair; the
-- fleet page shows the mute and lets an admin set one. The person '*' is the
-- fleet: it mutes the rows that name nobody (the missing-answer rate) and
-- the kind for everyone.
--
-- fleet_ticks is one row: when the evaluator last ticked, whichever instance
-- did. The advisory lock only stops two ticks that overlap; Cloud Run runs
-- several instances on their own timers, and each logged its own set of
-- fleet lines per interval, so the line-count metrics read as many fleets as
-- instances. A tick that finds this row younger than the interval is another
-- instance's interval and does nothing.
--
-- The seed below fills health_latest from the raw table once, so a machine
-- that reported before this revision and never again is not missing from the
-- fleet page on the first boot. Newest by the report's own clock, the rule
-- PutHealthReport's upsert keeps, so a queued sample delivered late does not
-- seed the row a newer sample would then refuse to replace. ON CONFLICT DO
-- NOTHING keeps it idempotent; a newer row written by PutHealthReport before
-- a re-run is never replaced.
--
-- Rollback floor: a revision from before this file writes health_reports
-- only. health_latest then lags until the newer revision returns and
-- PutHealthReport resumes upserting it; every raw row the old revision wrote
-- is still there to roll up. Nothing has to be undone.

CREATE TABLE IF NOT EXISTS health_latest (
  email         TEXT NOT NULL,
  device_id     UUID,
  emitted_at    TIMESTAMPTZ NOT NULL,
  received_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  worst         TEXT NOT NULL,
  agent_version TEXT,
  report        JSONB NOT NULL,
  -- A machine is (email, device_id), and a report that carried no device id
  -- is still one machine's: a constraint that treated two NULLs as distinct
  -- would store such a machine once per report. A primary key cannot hold
  -- a NULL, so the identity is a unique constraint with the NULLS NOT
  -- DISTINCT clause, which is what ON CONFLICT (email, device_id) needs.
  UNIQUE NULLS NOT DISTINCT (email, device_id)
);

CREATE TABLE IF NOT EXISTS health_hourly (
  email                TEXT NOT NULL,
  device_id            UUID,
  hour                 TIMESTAMPTZ NOT NULL,
  reports              INT NOT NULL DEFAULT 0,
  worst                TEXT NOT NULL DEFAULT 'info',
  conditions           JSONB NOT NULL DEFAULT '{}'::jsonb,
  drops                BIGINT NOT NULL DEFAULT 0,
  quarantined          BIGINT NOT NULL DEFAULT 0,
  -- parked and empty_starts are NULL for an hour built from version 1
  -- reports, which carry neither field: a reader of the ledger must be
  -- able to tell "the client could not say" from "the client said zero".
  parked               BIGINT,
  capture_blocked_secs INT NOT NULL DEFAULT 0,
  empty_starts         INT,
  UNIQUE NULLS NOT DISTINCT (email, device_id, hour)
);

CREATE INDEX IF NOT EXISTS health_reports_received_at_idx ON health_reports (received_at);

CREATE TABLE IF NOT EXISTS fleet_mutes (
  email      TEXT NOT NULL,
  kind       TEXT NOT NULL,
  until      TIMESTAMPTZ NOT NULL,
  note       TEXT,
  created_by TEXT,
  PRIMARY KEY (email, kind)
);

CREATE TABLE IF NOT EXISTS fleet_ticks (
  name         TEXT PRIMARY KEY,
  last_tick_at TIMESTAMPTZ NOT NULL
);

INSERT INTO health_latest (email, device_id, emitted_at, received_at, worst, agent_version, report)
SELECT DISTINCT ON (email, device_id)
       email, device_id, emitted_at, received_at, worst, report->>'agent_version', report
  FROM health_reports
 ORDER BY email, device_id, emitted_at DESC, id DESC
ON CONFLICT (email, device_id) DO NOTHING;
