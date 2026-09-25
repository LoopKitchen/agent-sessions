package slack

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DB is the connection source this package needs. A *pgxpool.Pool satisfies it
// as it stands, which is what the composition root hands over.
//
// The SQL in this file lives here rather than in server/store because these two
// tables are the mirror's own state and nothing else in the server reads them.
// server/store holds the corpus — the sessions, the events, the roster — and
// every package that touches it does so through a port it declares, because
// those rows are shared. slack_prefs and slack_posts are shared with nobody:
// they exist to decide what this package has already said. Keeping them here
// means the mirror is one directory and one migration, and switching it off is
// deleting both.
//
// It reads sessions, which it does not own, and only ever reads them.
type DB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Mode is where somebody's sessions are mirrored, if anywhere.
type Mode string

// The three modes, spelled exactly as internal/config spells them so that the
// client-side vocabulary and the server-side one are one vocabulary.
const (
	ModeOff     Mode = "off"
	ModeDM      Mode = "dm"
	ModeChannel Mode = "channel"
)

// Valid reports whether m is a mode this package will store.
func (m Mode) Valid() bool {
	switch m {
	case ModeOff, ModeDM, ModeChannel:
		return true
	}
	return false
}

// Prefs is one person's standing answer to "mirror my sessions where".
type Prefs struct {
	Email   string `json:"email"`
	Mode    Mode   `json:"mode"`
	Channel string `json:"channel,omitempty"`

	// SlackUserID is the resolved Slack account, cached at the moment the
	// preference was saved. Empty means it has not been resolved, which for DM
	// mode means the mirror will try again at post time.
	SlackUserID string `json:"slack_user_id,omitempty"`

	// MirrorFrom is the watermark: nothing that settled before it is ever
	// posted. See migration 0006 for why opting in must not post your history.
	MirrorFrom time.Time `json:"mirror_from"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Off reports the default. It is a method rather than a comparison at each call
// site so that "no row" and "mode off" cannot diverge.
func (p Prefs) Off() bool { return p.Mode != ModeDM && p.Mode != ModeChannel }

// readPrefs returns one person's preference, or the off default when they have
// never set one.
//
// Absence is not an error here, and that is the whole of the privacy default:
// somebody who has never heard of this feature has no row, and no row is off.
func readPrefs(ctx context.Context, db DB, email string) (Prefs, error) {
	p := Prefs{Email: email, Mode: ModeOff}
	err := db.QueryRow(ctx, `
		SELECT mode, coalesce(channel, ''), coalesce(slack_user_id, ''), mirror_from, updated_at
		  FROM slack_prefs
		 WHERE email = $1`, email,
	).Scan(&p.Mode, &p.Channel, &p.SlackUserID, &p.MirrorFrom, &p.UpdatedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Prefs{Email: email, Mode: ModeOff}, nil
	case err != nil:
		return Prefs{}, fmt.Errorf("slack: read preferences for %s: %w", email, err)
	}
	return p, nil
}

// savePrefs writes one person's preference and re-bases their watermark.
//
// mirror_from is set to now on every save that leaves the mirror on, including
// a change of destination. That is stricter than it needs to be for a first
// opt-in and it is deliberate for the rest: somebody moving their mirror from a
// DM to a channel has just changed the audience, and the sessions that settled
// while they were deciding were summarised for the old one. Re-basing means a
// destination only ever receives work that finished after somebody chose it.
//
// Turning the mirror off leaves the watermark alone, because it is about to be
// overwritten by whatever they turn on next.
func savePrefs(ctx context.Context, db DB, p Prefs, now time.Time) error {
	channel := strings.TrimSpace(p.Channel)
	if p.Mode != ModeChannel {
		// Cleared rather than kept, so a preference that reads "dm" cannot also
		// carry a channel somebody stopped using six months ago and forgot.
		channel = ""
	}
	_, err := db.Exec(ctx, `
		INSERT INTO slack_prefs (email, mode, channel, slack_user_id, mirror_from, updated_at)
		VALUES ($1, $2, nullif($3, ''), nullif($4, ''), $5, $5)
		ON CONFLICT (email) DO UPDATE SET
			mode          = EXCLUDED.mode,
			channel       = EXCLUDED.channel,
			slack_user_id = EXCLUDED.slack_user_id,
			mirror_from   = CASE WHEN EXCLUDED.mode = 'off'
			                     THEN slack_prefs.mirror_from
			                     ELSE EXCLUDED.mirror_from END,
			updated_at    = EXCLUDED.updated_at`,
		p.Email, string(p.Mode), channel, strings.TrimSpace(p.SlackUserID), now)
	if err != nil {
		return fmt.Errorf("slack: save preferences for %s: %w", p.Email, err)
	}
	return nil
}

// cacheUserID records a Slack account resolved after the preference was saved.
//
// Separate from savePrefs because it must not touch the watermark: resolving an
// id is the mirror catching up on work it could not do earlier, not the person
// changing their mind, and re-basing here would silently skip every session
// that settled while the lookup was failing.
func cacheUserID(ctx context.Context, db DB, email, userID string) error {
	_, err := db.Exec(ctx,
		`UPDATE slack_prefs SET slack_user_id = $2 WHERE email = $1`, email, userID)
	if err != nil {
		return fmt.Errorf("slack: cache the slack id for %s: %w", email, err)
	}
	return nil
}

// candidate is a session that may be due a message, with the preference that
// decides where it would go.
type candidate struct {
	Summary
	Mode        Mode
	Channel     string
	SlackUserID string
}

// window is the set of cutoffs one pass selects against. Computed by the caller
// from a single "now" so that every predicate in a pass agrees about when it is.
type window struct {
	// endedBefore is the settle point for a session we watched end: it must have
	// been quiet since this instant. Short, because the end marker has already
	// told us there is nothing more coming and the only thing left to wait for
	// is the tail of the same upload.
	endedBefore time.Time
	// quietBefore is the settle point for a session with no end marker, and it
	// is much further back. A session whose laptop simply stopped uploading for
	// ten minutes is very often a person at lunch, and summarising it as over
	// posts "went quiet" into a channel while they are still working in it.
	quietBefore time.Time
	// freshAfter is the floor on event time, and it is what stops a backfill
	// from becoming an announcement. A laptop that imports six months of history
	// delivers thousands of settled, unposted, substantial sessions in one go;
	// every one of them satisfies every other predicate here.
	freshAfter time.Time
	// leaseBefore is when an unfinished claim becomes reclaimable.
	leaseBefore time.Time

	maxAttempts int
	minTurns    int
	minTools    int
	limit       int
}

// eligible reports the sessions due a message, oldest first.
//
// The predicates are the whole policy, so they are worth reading as a set. A
// session is due when its owner has asked for a mirror, it settled after they
// asked, it has not been posted, it is recent, it is substantial, and it is not
// a subagent's.
//
// Subagents are excluded because they are not somebody's session: they are part
// of the session that dispatched them, they end when it is halfway through, and
// mirroring them separately turns one piece of work into a stream of messages
// about its own internals.
//
// The anti-join and the claim in claimKey must agree exactly about what a
// retryable row looks like. If this query offers a row the claim then refuses,
// the pass spins on it every minute; if the claim accepts a row this query
// never offers, a failed message is never retried at all.
func eligible(ctx context.Context, db DB, w window) ([]candidate, error) {
	rows, err := db.Query(ctx, `
		SELECT s.session_id, s.email, coalesce(s.repo, ''), coalesce(s.git_branch, ''),
		       s.started_at, coalesce(s.ended_at, s.started_at), s.ended,
		       s.user_turns, s.tool_calls, s.subagents, s.errors, s.cost_usd::float8,
		       p.mode, coalesce(p.channel, ''), coalesce(p.slack_user_id, '')
		  FROM sessions s
		  JOIN slack_prefs p ON p.email = s.email AND p.mode <> 'off'
		  LEFT JOIN slack_posts o ON o.key = 'session:' || s.session_id
		  LEFT JOIN slack_posts lt ON lt.key = 'thread:' || s.session_id
		 WHERE s.parent_session_id IS NULL
		   -- The store's default view, restated: a session in which nothing
		   -- happened and a harness helper run are not somebody's work, and a
		   -- digest about either would be a message about nothing. A row whose
		   -- device reported capture loss stays in whatever its type says.
		   AND NOT (s.session_type IN ('empty', 'internal') AND s.head_state <> 'capture_loss')
		   -- A session with a live thread gets its summary as that thread's
		   -- closing reply; a second standalone digest would say it twice.
		   AND lt.posted_at IS NULL
		   AND s.updated_at > p.mirror_from
		   -- The two settle points are cast explicitly, and the casts are load
		   -- bearing rather than decorative. A bare $1 inside a CASE gives
		   -- Postgres no context to infer a type from — the branches only have
		   -- to agree with each other — so it resolves both to text and then
		   -- refuses to compare the result with a timestamptz. The statement
		   -- fails at execution, every pass errors, and the mirror posts
		   -- nothing at all while looking perfectly well in every test that
		   -- does not use a real database.
		   AND s.updated_at <= CASE WHEN s.ended THEN $1::timestamptz ELSE $2::timestamptz END
		   AND coalesce(s.ended_at, s.started_at) >= $3
		   AND (s.user_turns >= $4 OR s.tool_calls >= $5)
		   AND (o.key IS NULL
		        OR (o.posted_at IS NULL AND o.claimed_at < $6 AND o.attempts < $7))
		 ORDER BY s.updated_at
		 LIMIT $8`,
		w.endedBefore, w.quietBefore, w.freshAfter,
		w.minTurns, w.minTools, w.leaseBefore, w.maxAttempts, w.limit)
	if err != nil {
		return nil, fmt.Errorf("slack: select the sessions due a message: %w", err)
	}
	defer rows.Close()

	var out []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(
			&c.SessionID, &c.Email, &c.Repo, &c.Branch,
			&c.StartedAt, &c.EndedAt, &c.Ended,
			&c.UserTurns, &c.ToolCalls, &c.Subagents, &c.Errors, &c.CostUSD,
			&c.Mode, &c.Channel, &c.SlackUserID,
		); err != nil {
			return nil, fmt.Errorf("slack: read a due session: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("slack: read the due sessions: %w", err)
	}
	return out, nil
}

// sessionKey is the deterministic id for a session's one message.
//
// Derived from the session id and nothing else, which is what makes it the same
// string on every instance, on every pass and after every restart. The events
// pipeline solves the same problem the same way, with the client's idempotency
// id; the difference is only that here the id is ours to derive because Slack
// has none of its own.
func sessionKey(sessionID string) string { return "session:" + sessionID }

// capKey is the deterministic id for the one notice a person gets on the day
// they reach the ceiling. Keyed by the day so tomorrow gets its own.
func capKey(email string, day time.Time) string {
	return "cap:" + email + ":" + day.UTC().Format("2006-01-02")
}

// claimKey takes exclusive ownership of a key, or reports that somebody else
// has it.
//
// This is where at-most-once is decided, and it is decided by the primary key
// rather than by anything this process remembers. Two instances sweeping at the
// same instant both run this statement; exactly one of them gets a row back.
//
// The conflict branch is the retry path, and its WHERE clause is the mirror of
// the anti-join in eligible: a row that has been posted is never re-claimed, a
// row somebody else claimed within the lease is left alone, and a row that has
// already failed its budget of attempts is abandoned rather than retried
// forever.
func claimKey(ctx context.Context, db DB, key, email, sessionID string, now, leaseBefore time.Time, maxAttempts int) (bool, error) {
	var attempts int
	err := db.QueryRow(ctx, `
		INSERT INTO slack_posts (key, email, session_id, claimed_at, attempts)
		VALUES ($1, $2, nullif($3, ''), $4, 1)
		ON CONFLICT (key) DO UPDATE
		   SET claimed_at = $4,
		       attempts   = slack_posts.attempts + 1
		 WHERE slack_posts.posted_at IS NULL
		   AND slack_posts.claimed_at < $5
		   AND slack_posts.attempts < $6
		RETURNING attempts`,
		key, email, sessionID, now, leaseBefore, maxAttempts,
	).Scan(&attempts)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("slack: claim %s: %w", key, err)
	}
	return true, nil
}

// markPosted settles a claim: this key has been said, here, and will never be
// said again.
func markPosted(ctx context.Context, db DB, key, channel, ts string, now time.Time) error {
	_, err := db.Exec(ctx, `
		UPDATE slack_posts
		   SET posted_at = $2, channel = $3, message_ts = $4, last_error = NULL
		 WHERE key = $1`, key, now, channel, ts)
	if err != nil {
		return fmt.Errorf("slack: record %s as posted: %w", key, err)
	}
	return nil
}

// markFailed records why a claim did not become a message.
//
// A permanent failure spends the whole attempt budget at once rather than
// waiting for three passes to discover the same answer. The row stays behind
// either way: a table of exactly the messages that were never sent, with Slack's
// own word for why, is the only place that question can be answered later.
func markFailed(ctx context.Context, db DB, key string, cause error, permanent bool, maxAttempts int) error {
	// Bounded, because Slack's error text is short but a wrapped transport
	// failure names hosts and can run long, and this column is read by a human
	// in a psql session.
	reason := clip(cause.Error(), 300)
	var err error
	if permanent {
		_, err = db.Exec(ctx,
			`UPDATE slack_posts SET last_error = $2, attempts = $3 WHERE key = $1`,
			key, reason, maxAttempts)
	} else {
		_, err = db.Exec(ctx,
			`UPDATE slack_posts SET last_error = $2 WHERE key = $1`, key, reason)
	}
	if err != nil {
		return fmt.Errorf("slack: record %s as failed: %w", key, err)
	}
	return nil
}

// postedSince counts what one person's mirror has actually sent in a window,
// which is what the daily ceiling is measured against.
//
// Claimed-but-unposted rows are not counted. The ceiling exists to protect the
// people reading the channel, and a message nobody received is not one of them.
func postedSince(ctx context.Context, db DB, email string, since time.Time) (int, error) {
	var n int
	if err := db.QueryRow(ctx,
		`SELECT count(*) FROM slack_posts WHERE email = $1 AND posted_at >= $2`,
		email, since,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("slack: count what %s has been sent: %w", email, err)
	}
	return n, nil
}
