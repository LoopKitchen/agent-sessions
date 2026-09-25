-- The Slack mirror: who has asked for one, and what has already been said.
--
-- Like every migration here, each statement is safe to re-run: a rolling deploy
-- starts several servers at once and each one applies this file.
--
-- Two tables and one index, and applying them mirrors nothing. The mirror is
-- off for everybody until a person turns it on for themselves through
-- PUT /v1/slack/preferences, because these messages are somebody's work posted
-- into a shared workspace: a default that broadcasts is a privacy incident that
-- ships as a feature. There is deliberately no INSERT below seeding anybody.

-- What one person has asked the mirror to do. One row per person, and a person
-- with no row is off — which is why nothing here defaults anybody in.
--
-- The preference lives server-side rather than in the agent's own config file,
-- where internal/config already carries an unread SlackPrefs. The poster runs
-- on the server, so a preference on a laptop would have to be uploaded to be
-- read, which makes the laptop's copy a cache of this one rather than the
-- source. It also makes the mode a machine-level setting: a person with two
-- laptops would be mirroring from one of them, and turning the mirror off would
-- mean remembering every machine they own.
CREATE TABLE IF NOT EXISTS slack_prefs (
  -- The roster address, and a foreign key to it. A preference belonging to
  -- nobody would keep posting after an offboarding removed the person: the
  -- mirror joins sessions to this table, and the join is the only thing between
  -- a departed colleague's late-arriving backfill and a channel.
  email          TEXT PRIMARY KEY REFERENCES principals(email) ON DELETE CASCADE,

  -- off, dm or channel. The same three words the client-side config uses, so a
  -- person reading either surface sees one vocabulary. Constrained rather than
  -- validated in Go alone: a typo'd mode that reached this column would be a
  -- row the poster silently never matches, which is indistinguishable from the
  -- feature not working.
  mode           TEXT NOT NULL DEFAULT 'off' CHECK (mode IN ('off', 'dm', 'channel')),

  -- Where channel mode posts. A Slack channel id (C…) or a name; the poster
  -- passes it to chat.postMessage as given.
  channel        TEXT,

  -- The person's Slack user id, resolved once from their email through
  -- users.lookupByEmail and cached here.
  --
  -- Cached because the lookup is the one call in this feature that can fail for
  -- a reason the person can act on — their Slack account carries a different
  -- address from their Workspace one — and a lookup performed at post time
  -- reports that failure into a log nobody reads. Resolving it when the
  -- preference is saved lets the answer come back in the response to the person
  -- who just asked for the mirror.
  slack_user_id  TEXT,

  -- The watermark the mirror starts from, set to now() every time somebody
  -- turns the mirror on.
  --
  -- Without it, opting in posts your history. The mirror's eligibility is "this
  -- session has settled and has not been posted", and on the day somebody
  -- enables it every session they have ever run satisfies both. That is the
  -- single most likely way this feature gets the bot muted, and it happens on
  -- the first person's first day rather than gradually.
  mirror_from    TIMESTAMPTZ NOT NULL DEFAULT now(),

  updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),

  -- Channel mode with no channel would be a preference that can never post.
  -- Refused here as well as in the handler, because the handler is not the only
  -- thing that will ever write this table — a psql session during an incident
  -- is the other one, and that is exactly when nobody is checking.
  CONSTRAINT slack_prefs_channel_required
    CHECK (mode <> 'channel' OR (channel IS NOT NULL AND channel <> ''))
);

-- What has been said, and the reason a retry cannot say it twice.
--
-- chat.postMessage has no idempotency key: send it the same message twice and
-- Slack posts it twice, cheerfully. So the guarantee has to be ours, and it is
-- built the way the events pipeline builds its own — on a deterministic id that
-- the database enforces. The events table keys on the client's idempotency id;
-- this table keys on a string derived from what the message is ABOUT, so the
-- same subject can only ever be claimed once no matter how many instances are
-- sweeping or how many times a pass is retried.
CREATE TABLE IF NOT EXISTS slack_posts (
  -- The deterministic id. 'session:<session_id>' for a session summary, and
  -- 'cap:<email>:<yyyy-mm-dd>' for the one notice a day that says the mirror
  -- has hit its own ceiling. Derived, never generated: a random id here would
  -- make every retry a new row and every new row a second message.
  key         TEXT PRIMARY KEY,

  -- Who the message is about, which is who the daily cap is counted against.
  email       TEXT NOT NULL,

  -- The session summarised, when there is one. Null on a cap notice.
  --
  -- ON DELETE CASCADE so retention carries these rows out with the sessions
  -- they describe, exactly as it already does for shares and search rows. The
  -- alternative is a table that outlives its subject forever and a mirror that
  -- gets slower every quarter.
  session_id  TEXT REFERENCES sessions(session_id) ON DELETE CASCADE,

  -- When this key was claimed, and by implication when the claim lapses.
  --
  -- A claim is taken before the Slack call and settled after it, so a process
  -- killed between the two leaves a row that is claimed and unposted. The
  -- sweeper reclaims one whose claim is older than its lease, which is what
  -- makes a mid-post crash recoverable at the cost of the one window in which
  -- this design can duplicate: killed after Slack accepted the message and
  -- before the row was settled. That window is a few milliseconds wide and its
  -- failure is a repeated message; the alternative — settle first, then post —
  -- loses messages instead, silently.
  claimed_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  posted_at   TIMESTAMPTZ,

  -- Where it went and what Slack called it. Neither is read by the poster; both
  -- are here because "the mirror says it posted and I never saw it" is
  -- otherwise unanswerable, and the channel a message landed in is the part
  -- somebody will want to check when it lands in the wrong one.
  channel     TEXT,
  message_ts  TEXT,

  -- Attempts is bounded, so a message Slack will never accept stops being
  -- retried every minute forever. last_error is the operator's copy of why.
  attempts    INT NOT NULL DEFAULT 1,
  last_error  TEXT
);

-- The daily cap counts one person's posted messages in a rolling window, and
-- the eligibility query runs it once per person per pass. Partial on posted_at
-- because a claimed-and-unposted row is not a message anybody received, and
-- because it keeps the index the size of what was actually sent.
CREATE INDEX IF NOT EXISTS slack_posts_email_posted_at_idx
  ON slack_posts (email, posted_at DESC) WHERE posted_at IS NOT NULL;

-- The eligibility query orders by sessions.updated_at and filters on it, and
-- until now nothing indexed that column: 0001 indexes started_at, which is
-- event time, and the mirror cannot use event time to decide what has settled.
-- A backfill delivers a session that ended last week, so by event time it is
-- instantly old, and by arrival time it has only just landed.
--
-- Built non-concurrently for the same reason 0005's index is: Migrate runs each
-- file in one transaction and CREATE INDEX CONCURRENTLY cannot run inside one.
-- sessions holds 892 rows today, so the lock this takes against the rollup is
-- measured in milliseconds. It is the events table that would make this a
-- hand-run statement instead, and this is not that table.
CREATE INDEX IF NOT EXISTS sessions_updated_at_idx ON sessions (updated_at);
