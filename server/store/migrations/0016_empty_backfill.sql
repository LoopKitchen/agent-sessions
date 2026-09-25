-- Retroactive classification of lifecycle-only sessions.
--
-- Its own file because it is the one migration that reads events. Every row
-- that has never counted a prompt, a tool call or a subagent is checked
-- against the events table for any content event at all; a row with none is
-- a session in which nothing happened and becomes empty. The check is a
-- per-session NOT EXISTS probe on events_session_id_seq_idx, never a GROUP BY
-- over the events table, with row locks on sessions only. Measured cost:
-- 3,344 rows reclassified in 4.5 s on a PITR clone of production during
-- the rehearsal, against the 3.6 s warm estimate the design's contract
-- matrix carries; both inside the boot
-- budget. The counters in the WHERE clause are what keep the probe to the
-- sessions that could be empty at all.
--
-- Rows that already have the counters are not touched, and a row that is
-- already empty is not touched, so re-running this file is a no-op; it is safe
-- to apply again after any rollback. empty_kind is provisional: an ended row
-- is called aborted, an un-ended one tail_truncated, and the derive runner's
-- session_class step refines both (blank, head_truncated, capture_loss) with
-- the device health and grace rules that need more than this table.
--
-- Rollback floor: a revision from before 0015 lists every type, so the rows
-- reclassified here simply reappear in its default view; nothing is lost.

UPDATE sessions
   SET session_type = 'empty',
       empty_kind = CASE WHEN ended THEN 'aborted' ELSE 'tail_truncated' END
 WHERE session_type = 'user'
   AND user_turns = 0 AND tool_calls = 0 AND subagents = 0
   AND NOT EXISTS (
     SELECT 1 FROM events e
      WHERE e.session_id = sessions.session_id
        AND e.type IN ('user_prompt', 'tool_call', 'tool_result', 'tool_failed',
                       'file_changed', 'assistant_turn', 'subagent_start', 'subagent_end'));
