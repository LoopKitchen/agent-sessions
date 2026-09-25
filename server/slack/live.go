package slack

import (
	"context"
	"log/slog"
	"time"
)

// The live pass: Devin-style threads for sessions that asked.
//
// Where the digest speaks once about a finished session, this speaks a few
// times about a running one: a root message when the session qualifies,
// a threaded reply per user turn and per milestone, a live-edited header on
// the root, and a closing summary when the session settles. Every message
// rides the same claims ledger as the digest, so at-most-once holds across
// passes, instances and restarts; the pacing mutex in the client is what
// keeps a busy thread under Slack's per-channel budget.

const (
	// liveTurnBatch bounds turn/milestone replies per session per PASS — a
	// drain rate, not a ceiling. There is no per-thread or per-day cap by the
	// owner's explicit decision (2026-08-13): a thread that follows a real
	// conversation is bounded by the conversation, exchanges arrive at most
	// this many per minute, and the client's pacing mutex holds every channel
	// under Slack's own budget. The caps this replaced silenced a real
	// session mid-flight and told its owner about a limit she never asked
	// for.
	liveTurnBatch = 10
	// liveFresh is the floor on session activity for a live thread. Tighter
	// than the digest's maxAge on purpose: "live" a day quiet is an archive.
	liveFresh = 12 * time.Hour
)

// LivePass is what one live sweep did.
type LivePass struct {
	Watching int
	Opened   int
	Replies  int
	Closed   int
	Failed   int
	Capped   int
}

// LogValue keeps a pass to one structured field.
func (p LivePass) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Int("watching", p.Watching),
		slog.Int("opened", p.Opened),
		slog.Int("replies", p.Replies),
		slog.Int("closed", p.Closed),
		slog.Int("failed", p.Failed),
		slog.Int("capped", p.Capped),
	)
}

// livePass advances every watched thread one step: first the group
// activations (the modern path), then the legacy env-flag sessions that have
// no activations. A session flagged 'on' whose owner has a default group is
// promoted into an activation here, which is how the old flag flows into the
// new vocabulary without a migration.
func (m *Mirror) livePass(ctx context.Context) (LivePass, error) {
	var p LivePass
	now := m.now()

	if err := m.promoteDefaults(ctx, now); err != nil {
		m.log.Warn("slack live default-group promotion failed", "err", err)
	}

	acts, err := groupEligible(ctx, m.db, now.Add(-liveFresh), m.batch)
	if err != nil {
		return p, err
	}
	p.Watching = len(acts)
	for _, a := range acts {
		if ctx.Err() != nil {
			return p, ctx.Err()
		}
		spent, err := m.advanceThread(ctx, &a.liveCandidate, threadCtx{
			prefix:      groupThreadPrefix(a.SessionID, a.GroupID),
			destination: a.Destination,
			groupID:     a.GroupID,
			after:       a.AttachedAt,
		}, now)
		p.Opened += spent.Opened
		p.Replies += spent.Replies
		p.Closed += spent.Closed
		p.Failed += spent.Failed
		p.Capped += spent.Capped
		if err != nil {
			m.log.Warn("slack group thread stalled",
				"session_id", a.SessionID, "group", a.GroupName, "err", err)
		}
	}

	due, err := liveEligible(ctx, m.db, now.Add(-liveFresh), m.batch)
	if err != nil {
		return p, err
	}
	p.Watching += len(due)

	for _, c := range due {
		if ctx.Err() != nil {
			return p, ctx.Err()
		}
		spent, err := m.advanceThread(ctx, &c, threadCtx{
			prefix:  threadPrefix(c.SessionID),
			request: c.Request,
		}, now)
		p.Opened += spent.Opened
		p.Replies += spent.Replies
		p.Closed += spent.Closed
		p.Failed += spent.Failed
		p.Capped += spent.Capped
		if err != nil {
			m.log.Warn("slack live thread stalled",
				"session_id", c.SessionID, "email", c.Email, "err", err)
		}
	}
	return p, nil
}

// threadCtx names one thread: its ledger namespace and where it posts. The
// group path supplies a destination outright; the legacy path supplies the
// raw request and falls back to the standing preference the way it always
// has.
type threadCtx struct {
	prefix      string
	destination string // group path: 'dm' or a channel id
	request     string // legacy path: 'on' | 'dm' | channel id
	groupID     int64  // group path only; powers the Stop button's identity
	// after is the history horizon: turns before it are never posted. The
	// group path sets it to the attachment moment; the legacy path leaves it
	// zero because those sessions asked at birth.
	after time.Time
}

// advanceThread advances a single thread and reports what it spent.
func (m *Mirror) advanceThread(ctx context.Context, c *liveCandidate, tc threadCtx, now time.Time) (LivePass, error) {
	var p LivePass

	channel := ""
	switch {
	case tc.destination != "" && tc.destination != "dm":
		channel = tc.destination
	case tc.destination == "dm":
		var err error
		channel, err = m.destination(ctx, candidate{
			Summary:     Summary{SessionID: c.SessionID, Email: c.Email},
			Mode:        ModeDM,
			SlackUserID: c.SlackUserID,
		})
		if err != nil {
			p.Failed++
			return p, err
		}
	case tc.request != "" && tc.request != "on" && tc.request != "dm":
		channel = tc.request
	default:
		var err error
		channel, err = m.destination(ctx, candidate{
			Summary:     Summary{SessionID: c.SessionID, Email: c.Email},
			Mode:        c.Mode,
			Channel:     c.Channel,
			SlackUserID: c.SlackUserID,
		})
		if err != nil {
			p.Failed++
			return p, err
		}
	}

	// Open the root once the session shows substance — the digest's own
	// triviality bar — so a pong ping that asked (via a stale exported env
	// var) never opens an empty thread.
	if c.RootTS == "" {
		if c.UserTurns < m.minTurns && c.ToolCalls < m.minTools {
			return p, nil
		}
		key := liveKey(tc.prefix)
		claimed, err := claimKey(ctx, m.db, key, c.Email, c.SessionID, now, now.Add(-m.lease), m.maxAttempts)
		if err != nil || !claimed {
			return p, err
		}
		rootText := liveRootText(*c, m.publicURL)
		var ts string
		if m.Interactive() && tc.groupID > 0 {
			ts, err = m.slack.PostMessageBlocks(ctx, channel, rootText,
				liveRootBlocks(rootText, c.SessionID, tc.groupID))
		} else {
			ts, err = m.post(ctx, channel, rootText)
		}
		if err != nil {
			p.Failed++
			return p, m.recordFailure(ctx, key, err)
		}
		if err := markPosted(ctx, m.db, key, channel, ts, now); err != nil {
			return p, err
		}
		c.RootTS, c.RootChannel = ts, channel
		p.Opened++
	}
	if c.RootChannel != "" {
		channel = c.RootChannel
	}

	// Exchanges, then milestones, each idempotent through its own key. No
	// thread cap and no daily cap — the batch is a per-pass drain rate and
	// the client's pacing is the throughput bound; see the constants block.
	batch := liveTurnBatch
	turns, err := liveTurns(ctx, m.db, c.SessionID, tc.prefix, tc.after, batch)
	if err != nil {
		return p, err
	}
	for _, t := range turns {
		if err := m.liveReply(ctx, c, channel, liveTurnKey(tc.prefix, t.Seq), liveTurnText(t, c.Email), now, &p); err != nil {
			return p, err
		}
	}
	miles, err := liveMilestones(ctx, m.db, c.SessionID, tc.prefix, tc.after, batch)
	if err != nil {
		return p, err
	}
	for _, mi := range miles {
		if err := m.liveReply(ctx, c, channel, liveMilestoneKey(tc.prefix, mi.About), liveMilestoneText(mi), now, &p); err != nil {
			return p, err
		}
	}

	// Settle or refresh the header. The same two-tier quiet rule as the
	// digest: an announced end settles fast, silence settles slow.
	quietFor := now.Sub(c.UpdatedAt)
	switch {
	case c.Ended && quietFor >= m.settleEnded:
		return p, m.liveClose(ctx, c, tc, channel, now, &p, "finished")
	case quietFor >= m.quietFor:
		return p, m.liveClose(ctx, c, tc, channel, now, &p, "quiet")
	default:
		// A live edit is idempotent by nature; no claim, best-effort.
		if err := m.updateRoot(ctx, channel, c, tc, ":large_green_circle:"); err != nil {
			m.log.Warn("slack live header update failed", "session_id", c.SessionID, "err", err)
		}
	}
	return p, nil
}

// liveReply posts one threaded reply under its deterministic key.
func (m *Mirror) liveReply(ctx context.Context, c *liveCandidate, channel, key, text string, now time.Time, p *LivePass) error {
	claimed, err := claimKey(ctx, m.db, key, c.Email, c.SessionID, now, now.Add(-m.lease), m.maxAttempts)
	if err != nil || !claimed {
		return err
	}
	ts, err := m.slack.PostThreadReply(ctx, channel, c.RootTS, text)
	if err != nil {
		p.Failed++
		return m.recordFailure(ctx, key, err)
	}
	p.Replies++
	return markPosted(ctx, m.db, key, channel, ts, now)
}

// liveClose ends the thread: the digest summary as the final reply, and the
// root edited to its terminal state. Claimed once; the digest pass excludes
// sessions with a live root, so the summary is said exactly here.
func (m *Mirror) liveClose(ctx context.Context, c *liveCandidate, tc threadCtx, channel string, now time.Time, p *LivePass, why string) error {
	key := liveCloseKey(tc.prefix)
	claimed, err := claimKey(ctx, m.db, key, c.Email, c.SessionID, now, now.Add(-m.lease), m.maxAttempts)
	if err != nil || !claimed {
		return err
	}
	s := Summary{
		SessionID: c.SessionID, Email: c.Email, Repo: c.Repo, Branch: c.Branch,
		StartedAt: c.StartedAt, EndedAt: c.UpdatedAt, Ended: c.Ended,
		UserTurns: c.UserTurns, ToolCalls: c.ToolCalls, Errors: c.Errors, CostUSD: c.CostUSD,
	}
	text := render(s, m.publicURL, ModeChannel)
	if why == "capped" {
		text = ":no_bell: This thread reached its message cap; the rest of the session is on the dashboard.\n" + text
	}
	ts, err := m.slack.PostThreadReply(ctx, channel, c.RootTS, text)
	if err != nil {
		p.Failed++
		return m.recordFailure(ctx, key, err)
	}
	if err := markPosted(ctx, m.db, key, channel, ts, now); err != nil {
		return err
	}
	p.Closed++
	status := map[string]string{"finished": ":white_check_mark:", "quiet": ":zzz:"}[why]
	// The closed root goes back to plain text ON PURPOSE: the mirroring is
	// over, so a Stop button would be an offer the server can only refuse.
	if err := m.slack.UpdateMessage(ctx, channel, c.RootTS, liveHeaderText(*c, m.publicURL, status)); err != nil {
		m.log.Warn("slack live final header update failed", "session_id", c.SessionID, "err", err)
	}
	return nil
}

// updateRoot refreshes the live header. chat.update replaces the whole
// message, so an interactive root must have its Stop button re-sent with
// every edit or the first heartbeat strips it.
func (m *Mirror) updateRoot(ctx context.Context, channel string, c *liveCandidate, tc threadCtx, status string) error {
	text := liveHeaderText(*c, m.publicURL, status)
	if m.Interactive() && tc.groupID > 0 {
		return m.slack.UpdateMessageBlocks(ctx, channel, c.RootTS, text,
			liveRootBlocks(text, c.SessionID, tc.groupID))
	}
	return m.slack.UpdateMessage(ctx, channel, c.RootTS, text)
}

// promoteDefaults turns flag spellings into activations: 'on' becomes the
// owner's default group where one is set, and a group NAME becomes that
// group, preferring the owner's own over an org one of the same name. The
// flag keeps its old meaning for everybody else, and a name that resolves to
// nothing posts nowhere — the legacy path refuses name shapes outright, so a
// typo is silence rather than a message to a channel called "myteam".
func (m *Mirror) promoteDefaults(ctx context.Context, now time.Time) error {
	if _, err := m.db.Exec(ctx, `
		INSERT INTO session_mirrors (session_id, group_id, attached_by, attached_at)
		SELECT s.session_id, p.default_group, s.email, $2
		  FROM sessions s
		  JOIN slack_prefs p ON p.email = s.email AND p.default_group IS NOT NULL
		 WHERE s.mirror_request = 'on'
		   AND s.parent_session_id IS NULL
		   AND s.started_at >= $1
		   AND NOT EXISTS (SELECT 1 FROM session_mirrors sm
		                    WHERE sm.session_id = s.session_id AND sm.group_id = p.default_group)
		ON CONFLICT (session_id, group_id) DO NOTHING`, now.Add(-liveFresh), now); err != nil {
		return err
	}
	_, err := m.db.Exec(ctx, `
		INSERT INTO session_mirrors (session_id, group_id, attached_by, attached_at)
		SELECT DISTINCT ON (s.session_id) s.session_id, g.id, s.email, $2
		  FROM sessions s
		  JOIN slack_groups g
		    ON g.name = s.mirror_request
		   AND (g.owner_email = s.email OR g.visibility = 'org')
		   AND NOT g.disabled
		 WHERE s.mirror_request NOT IN ('', 'on', 'dm')
		   AND s.mirror_request !~ '^[CG][A-Z0-9]{6,20}$'
		   AND s.parent_session_id IS NULL
		   AND s.started_at >= $1
		 ORDER BY s.session_id, g.owner_email <> s.email
		ON CONFLICT (session_id, group_id) DO NOTHING`, now.Add(-liveFresh), now)
	return err
}
