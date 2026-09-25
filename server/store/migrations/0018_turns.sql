-- Turns: one row per logical exchange, whichever capture path recorded it.
--
-- The reader, the list counters, the Slack mirror and the export all want the
-- same thing and each derived it differently from raw events: what the
-- person asked, what the agent did, what it finally said, and how long that
-- took. A session captured both live and from its transcript holds every
-- prompt twice, so every consumer either double counted or carried its own
-- dedupe. This table is the one fold. It is derived, by the runner alone,
-- from the column projection of events (never from bodies), and it can be
-- discarded and rebuilt at any time; nothing in it is a second source of
-- truth.
--
-- Identity. (session_id, thread, turn_index) is the position a reader pages
-- by. (session_id, thread, turn_key) is the invariant: the key names the
-- exchange across origins (pid:<prompt_id> when either copy carries the
-- harness's prompt id, ts:<second>:<hash> otherwise), so two copies of one
-- prompt cannot become two rows. The fold appends an ordinal to a key that
-- would otherwise repeat, so a collision is a suffix and never a failed
-- write. thread is '' for the main conversation and the canonical agent id
-- for a subagent's stream.
--
-- prompt_event_id and final_event_id name the canonical copies: the
-- transcript row for text where both origins have one. outcome says what
-- became of the exchange. inherited marks a turn a fork copied from its
-- origin session; it is stored, because a resume needs the prefix, and
-- flagged, so a count does not attribute the same work to two sessions.
-- origins records which capture paths contributed; merged counts the hook
-- copies that folded into transcript ones; prompts counts the prompt groups
-- in the turn (the opener plus any notification that folded into it), which
-- is what a session's user_turns is the sum of.
--
-- turn_events maps every event to its turn and says what it is there: the
-- canonical prompt, the canonical final answer, work, or a copy the fold
-- elected out (a hook prompt whose transcript twin is canonical, or a
-- transcript tool result whose hook twin carries the fuller output). The
-- reader renders from this mapping rather than from the superseded_by marker
-- on the event row, so "hook is canonical for tool output" is expressed here
-- without ever marking a transcript row.
--
-- Both tables cascade from sessions, so retention's session delete takes them
-- with it. turn_events has no foreign key to events on purpose: retention
-- deletes events in bounded batches before the session row, and a foreign key
-- there would make every one of those deletes check this table.
--
-- No secondary index. The primary keys are the access paths the runner and
-- the reader use; anything a later reader needs is built by the runner
-- CONCURRENTLY, outside the migration lock.
--
-- Rollback floor: a revision from before this file does not know the tables
-- exist. They sit unread and unwritten until the newer revision returns, and
-- the runner's turns step re-derives every session then. Nothing is lost.

CREATE TABLE IF NOT EXISTS turns (
  session_id           TEXT NOT NULL REFERENCES sessions(session_id) ON DELETE CASCADE,
  thread               TEXT NOT NULL DEFAULT '',
  turn_index           INT  NOT NULL,
  turn_key             TEXT NOT NULL,
  kind                 TEXT NOT NULL DEFAULT '',
  prompt_event_id      TEXT,
  final_event_id       TEXT,
  outcome              TEXT NOT NULL DEFAULT 'in_progress'
    CHECK (outcome IN ('answered', 'interrupted', 'no_answer_captured', 'no_work', 'in_progress')),
  inherited            BOOLEAN NOT NULL DEFAULT false,
  started_at           TIMESTAMPTZ NOT NULL,
  first_activity_at    TIMESTAMPTZ,
  last_activity_at     TIMESTAMPTZ NOT NULL,
  answered_at          TIMESTAMPTZ,
  wall_ms              BIGINT NOT NULL DEFAULT 0,
  active_ms            BIGINT NOT NULL DEFAULT 0,
  idle_ms              BIGINT NOT NULL DEFAULT 0,
  waiting_for_human_ms BIGINT NOT NULL DEFAULT 0,
  origins              TEXT[] NOT NULL DEFAULT '{}',
  merged               INT NOT NULL DEFAULT 0,
  prompts              INT NOT NULL DEFAULT 1,
  tool_calls           INT NOT NULL DEFAULT 0,
  errors               INT NOT NULL DEFAULT 0,
  subagents            INT NOT NULL DEFAULT 0,
  files_changed        INT NOT NULL DEFAULT 0,
  tokens_input         BIGINT NOT NULL DEFAULT 0,
  tokens_output        BIGINT NOT NULL DEFAULT 0,
  tokens_cache_read    BIGINT NOT NULL DEFAULT 0,
  tokens_cache_write   BIGINT NOT NULL DEFAULT 0,
  cost_usd             NUMERIC(12,6) NOT NULL DEFAULT 0,
  model                TEXT NOT NULL DEFAULT '',
  derived_version      INT NOT NULL DEFAULT 0,
  derived_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (session_id, thread, turn_index),
  UNIQUE (session_id, thread, turn_key)
);

CREATE TABLE IF NOT EXISTS turn_events (
  session_id TEXT NOT NULL,
  thread     TEXT NOT NULL DEFAULT '',
  turn_index INT  NOT NULL,
  event_id   TEXT NOT NULL,
  role       TEXT NOT NULL CHECK (role IN ('prompt', 'final', 'work', 'superseded')),
  PRIMARY KEY (session_id, thread, turn_index, event_id),
  FOREIGN KEY (session_id, thread, turn_index)
    REFERENCES turns(session_id, thread, turn_index) ON DELETE CASCADE
);
