-- Chronological transcript reads.
--
-- Event seq is per-stream: the walker numbers each subagent's file from zero so
-- ids stay window-independent, which means ORDER BY seq round-robins every
-- thread of a multi-agent session (all the seq-0 rows first, then the seq-1
-- rows). The transcript page reads in time order instead, with the agent id as
-- the tiebreak so one thread's simultaneous rows stay contiguous. coalesce
-- matches the read's ORDER BY exactly; a NULL agent_id must sort with '' or
-- keyset pagination skips rows at the boundary.

CREATE INDEX IF NOT EXISTS events_session_timeline_idx
  ON events (session_id, occurred_at, coalesce(agent_id, ''), seq);
