-- Message kinds and title provenance: columns only.
--
-- messages.kind is the one persisted classification of a user-role message
-- (internal/normalize decides it; see that package for the vocabulary). Until
-- now the title, the turn counter, search and the Slack mirror each guessed
-- what a person said from the text, and guessed differently. The kind lands
-- here at ingest from this migration on; nothing else reads it until the
-- readers are moved onto it.
--
-- messages.agent_id repeats the event's thread. The first prompt of a session
-- has to come from the MAIN thread, and messages carried no way to say which
-- thread a row belonged to, so a subagent's task prompt (its own stream starts
-- at seq 1) could win the title over the person's opening prompt.
--
-- Back-population of kind and agent_id for the rows already stored is NOT done
-- here. It is a read of every message row's text, which is minutes of work
-- under the migration lock; the derive runner's messages_kind step does it in
-- resumable batches after boot. Until that step has run, every historical row
-- carries kind '' and agent_id NULL, and readers must treat '' as "not yet
-- classified" rather than as "human".
--
-- sessions.title_source records how first_prompt was chosen, so a reader can
-- tell a title a person typed from a template that named an automation run,
-- and a list can badge one and not the other. human_turns counts main-thread
-- prompts a person typed (kind human or slash_command), which user_turns,
-- counting every user_prompt event, never could. harness_title keeps the
-- harness's own generated title as a secondary label. agent_versions records
-- which client builds delivered the session, from the ingest User-Agent, which
-- is the cohort key for "did the new client capture the answer".
--
-- Every ADD COLUMN carries a constant default or none, so each is a catalog
-- change on Postgres 11+ with no table rewrite; messages is the largest table
-- this touches and it is not scanned. The CHECK on title_source is validated
-- against sessions (thousands of rows), not messages.
--
-- Rollback floor: a server revision from before this file ignores every
-- column here and keeps writing first_prompt the old way. Nothing has to be
-- undone; the columns sit unused until the newer revision returns.

ALTER TABLE messages ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT '';
ALTER TABLE messages ADD COLUMN IF NOT EXISTS agent_id TEXT;

ALTER TABLE sessions ADD COLUMN IF NOT EXISTS title_source TEXT NOT NULL DEFAULT 'none'
  CHECK (title_source IN ('none', 'human', 'command', 'automation_template'));
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS human_turns INT NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS harness_title TEXT;
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS agent_versions TEXT[];
