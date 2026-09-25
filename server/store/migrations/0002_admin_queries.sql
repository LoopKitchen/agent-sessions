-- Storage and access paths the admin surface needs.
--
-- Like 0001, every statement here is safe to re-run: a rolling deploy starts
-- several servers at once and each one runs migrations.

-- The trail for role and status edits. access_log cannot carry these because its
-- rows are session-scoped and a role change has no session, and the two trails
-- answer different questions anyway: access_log says who read whose work, this
-- says who was given the standing right to.
--
-- from_role is nullable and NULL means the principal did not exist before the
-- change. That is the only record distinguishing a creation from a promotion
-- after the fact, and it is stored as the absence of a value rather than as a
-- second boolean so the two cannot end up disagreeing.
CREATE TABLE IF NOT EXISTS principal_changes (
  id            BIGSERIAL PRIMARY KEY,
  actor         TEXT NOT NULL,
  target        TEXT NOT NULL,
  from_role     TEXT,
  to_role       TEXT NOT NULL,
  from_disabled BOOLEAN NOT NULL,
  to_disabled   BOOLEAN NOT NULL,
  at            TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- The two questions asked of this table are "how did this person get access"
-- and "what has this admin been granting", and both are asked newest first.
CREATE INDEX IF NOT EXISTS principal_changes_target_at_idx ON principal_changes (target, at DESC);
CREATE INDEX IF NOT EXISTS principal_changes_actor_at_idx ON principal_changes (actor, at DESC);

-- Coverage reads the newest report per (email, device_id) on every page load,
-- and the existing index is keyed on (email, emitted_at) which serves neither
-- half of that: it does not group by device, and emitted_at is the reporting
-- machine's clock rather than arrival, so it is not the column the pick orders
-- by. Without this the fleet page sorts the entire report history to find one
-- row per laptop, and that history grows by one row per machine per interval
-- forever.
CREATE INDEX IF NOT EXISTS health_reports_email_device_received_at_idx
  ON health_reports (email, device_id, received_at DESC, id DESC);

-- "Who has been reading this person's sessions" is one of the two questions the
-- audit log exists to answer, and it was the one with no index: 0001 covers
-- session_id and viewer but not owner, leaving that filter as a scan of the
-- largest append-only table after events.
CREATE INDEX IF NOT EXISTS access_log_owner_at_idx ON access_log (owner, at DESC);
