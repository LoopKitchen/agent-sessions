// Package slack mirrors a person's finished sessions into Slack, so that
// following somebody's work does not require opening the dashboard.
//
// Four decisions shape everything here, and each of them is the answer to a way
// this feature goes wrong rather than a preference.
//
// # One message per session, when the session is over
//
// A message per event is a firehose: the busiest laptop on file produced 71,700
// events in nine days, and a bot that posts at that rate is muted on its first
// afternoon. A message per session is the unit somebody actually follows, and
// the end of one is the moment it can be described — a session that has just
// started has no counts, no duration and nothing to say beyond that it exists.
//
// The end is not always announced, so it is inferred twice over. A session
// carrying an end marker settles quickly. A session that merely stopped
// arriving waits far longer, because a laptop that went quiet for ten minutes is
// very often somebody at lunch, and the message says "went quiet" rather than
// "finished" — internal/event draws that distinction deliberately and this
// surface must not collapse it.
//
// Volume is bounded three more times after that. Subagent sessions are not
// mirrored, because they are part of the session that dispatched them. Trivial
// sessions are not mirrored: on the live corpus, 892 sessions in nine days
// contain 128 with two turns or ten tool calls, and the remainder are one-turn
// pipeline calls that nobody follows. And a person's mirror stops for the day at
// a ceiling, with one message saying so, because a feed that goes silent for a
// reason nobody can see is indistinguishable from one that broke.
//
// # Off for everybody, per person, server-side
//
// This posts somebody's work into a shared workspace. A default that broadcasts
// is a privacy incident that ships as a feature, so the preference defaults to
// off, is set per person by that person, and has no company-wide switch that
// turns it on for anybody else.
//
// It lives in the database rather than in the agent's config file — where
// internal/config already carries a SlackPrefs that nothing reads — because the
// poster is server-side. A preference on a laptop would have to be uploaded to
// be acted on, which makes the laptop's copy a cache; it would also make the
// setting per-machine, so somebody with two laptops would mirror from one of
// them and would have to remember every machine they own to turn it off.
//
// # Metadata only, never transcript — with one bounded exception
//
// The live thread is the exception, and its boundary is consent: a session
// mirrored live was opted in EXPLICITLY, for exactly that session, by the
// person who owns it (an env flag at launch or a deliberate mid-session
// call). For those sessions and only those, a turn reply carries the
// prompt's first line, clipped — the same line the dashboard's own list
// shows. The standing digest keeps the stricter rule below, because a
// standing preference is consent to be summarised, not to be quoted.
//
// A digest message carries what was worked on, how much of it there was, and a link.
// It never carries a prompt, a tool call, a file path or a model response. The
// dashboard decides who may read a transcript, records every read of somebody
// else's, and can expire the body under a retention policy; a Slack message is
// subject to none of that, and is readable by everybody in the channel now,
// everybody who joins later, and every export of the workspace, with no record
// that anybody read it. Summary is where that boundary is enforced and where the
// argument is written down.
//
// # A retry must not post twice
//
// Slack's chat.postMessage has no idempotency key, so the guarantee is ours. It
// is built the way the events pipeline builds its own: on a deterministic id the
// database enforces. Every message has a key derived from what it is about —
// "session:<id>" — and slack_posts.key is a primary key, so a claim is a race
// exactly one caller can win, across passes, across instances and across
// restarts.
package slack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// The mirror's schedule and thresholds. Every one of them is overridable
// through Options, and every default is the number the live corpus argued for.
const (
	// defaultInterval is the gap between passes. A pass with nothing to do is
	// one indexed query, and when nobody has opted in the join against
	// slack_prefs matches no rows at all, so the idle cost of this feature is a
	// query per minute that returns nothing.
	defaultInterval = time.Minute

	// defaultSettleEnded is how long a session that announced its end must have
	// been quiet before it is summarised. Short: the marker has already said
	// nothing more is coming, and the only thing left to wait for is the tail of
	// the upload carrying it.
	defaultSettleEnded = 5 * time.Minute

	// defaultQuietFor is the same wait for a session that never announced
	// anything. Long, because the cost of being wrong is posting "went quiet"
	// about a session somebody is still working in. Three quarters of an hour is
	// longer than a meeting and longer than lunch.
	defaultQuietFor = 45 * time.Minute

	// defaultMaxAge is the floor on event time, and it is what stands between a
	// backfill and a channel. A laptop importing six months of history delivers
	// thousands of sessions that are settled, unposted, substantial and owned by
	// somebody who has opted in. Every other predicate here admits all of them.
	defaultMaxAge = 24 * time.Hour

	// defaultLease is how long a claim is honoured before another pass may take
	// it. It only matters when a process dies between claiming a key and posting
	// it, which is the window this design trades a duplicate for a loss in.
	defaultLease = 5 * time.Minute

	// defaultMaxAttempts is how many times one message may be attempted before
	// it is abandoned. Five passes of a transient Slack failure is enough to
	// cross any rate limit; past that the message is stale anyway.
	defaultMaxAttempts = 5

	// defaultBatch bounds one pass. A backlog drains over several passes rather
	// than in one burst, which is what keeps a recovery from looking like the
	// firehose this design exists to avoid.
	defaultBatch = 25

	// defaultDailyCap is the ceiling on one person's messages in a rolling day.
	// The busiest day on file yields 41 eligible sessions, so this is headroom
	// rather than a limit anybody meets; it is here for the day a new agent
	// version changes what a session is.
	defaultDailyCap = 50

	// The substance thresholds. Either one qualifies, because the two shapes of
	// real work look nothing alike: a conversation is turns with few tools, and
	// a long autonomous run is one turn and hundreds of tools.
	defaultMinTurns = 2
	defaultMinTools = 10
)

// Poster is the part of *Client the mirror uses. It is declared for the one
// place a test needs to make Slack fail in a way an httptest server cannot —
// there is no fake in this package that implements it, because every test here
// drives the real client against a fake Slack instead.
type Poster interface {
	PostMessage(ctx context.Context, channel, text string) (string, error)
	PostMessageBlocks(ctx context.Context, channel, text, blocksJSON string) (string, error)
	PostThreadReply(ctx context.Context, channel, threadTS, text string) (string, error)
	UpdateMessage(ctx context.Context, channel, ts, text string) error
	UpdateMessageBlocks(ctx context.Context, channel, ts, text, blocksJSON string) error
	JoinChannel(ctx context.Context, channel string) error
	LookupUserByEmail(ctx context.Context, email string) (string, error)
	OpenDM(ctx context.Context, userID string) (string, error)
}

// Options configure a Mirror.
type Options struct {
	// DB is the connection source. Required.
	DB DB
	// Slack is the Web API client. Required.
	Slack Poster

	// PublicURL is the dashboard's absolute base, and the link in every message
	// is built from it. Required: a summary with no way back to the session is
	// the one thing this feature must never post, because the link is what keeps
	// the permission model in the loop.
	PublicURL string

	// Viewer resolves the person behind a request on the preferences route, and
	// reports false when there is nobody. Required — a nil viewer would leave
	// somebody's mirror settable by anybody who can reach the port.
	Viewer func(*http.Request) (string, bool)

	// DeviceEmail resolves the person behind a device token, for the routes
	// the agent CLI calls. Optional: without it those routes accept only the
	// browser cookie.
	DeviceEmail func(*http.Request) (string, bool)

	// SigningSecret verifies Slack's interactive payloads. Optional: absent,
	// the interactive route is not mounted and roots carry no buttons.
	SigningSecret string

	// Every threshold below defaults to the constant of the same name. They are
	// options so the tests can drive a pass in milliseconds, and so a deployment
	// that finds the cadence wrong can change it without a release.
	Interval    time.Duration
	SettleEnded time.Duration
	QuietFor    time.Duration
	MaxAge      time.Duration
	Lease       time.Duration
	MaxAttempts int
	Batch       int
	DailyCap    int
	MinTurns    int
	MinTools    int

	// Now is the clock every predicate in a pass is computed from.
	Now func() time.Time

	Logger *slog.Logger
}

// Mirror posts session summaries and serves the preference that asks for them.
//
// One type for both halves on purpose. The poster and the preference are the
// same feature seen from two directions, and splitting them would let a server
// mount the route that turns the mirror on without starting the loop that acts
// on it — which is the failure this codebase has produced six times over.
type Mirror struct {
	db            DB
	slack         Poster
	publicURL     string
	viewer        func(*http.Request) (string, bool)
	deviceEmail   func(*http.Request) (string, bool)
	signingSecret string
	chanCache     channelCache
	log           *slog.Logger
	now           func() time.Time

	interval    time.Duration
	settleEnded time.Duration
	quietFor    time.Duration
	maxAge      time.Duration
	lease       time.Duration
	maxAttempts int
	batch       int
	dailyCap    int
	minTurns    int
	minTools    int
}

// New builds a Mirror, refusing anything it would only discover later.
//
// Every missing dependency here fails at boot rather than at the first pass,
// because the first pass happens a minute after a deploy with nobody watching,
// and its failure is a log line in the middle of ordinary traffic.
func New(o Options) (*Mirror, error) {
	if o.DB == nil {
		return nil, errors.New("slack: DB is required")
	}
	if o.Slack == nil {
		return nil, errors.New("slack: a Slack client is required")
	}
	if o.PublicURL == "" {
		return nil, errors.New("slack: PublicURL is required; a summary with no link back is not one this package will post")
	}
	if o.Viewer == nil {
		return nil, errors.New("slack: Viewer is required; without it the preferences route would be open")
	}

	m := &Mirror{
		db:            o.DB,
		slack:         o.Slack,
		publicURL:     o.PublicURL,
		viewer:        o.Viewer,
		deviceEmail:   o.DeviceEmail,
		signingSecret: o.SigningSecret,
		log:           o.Logger,
		now:           o.Now,
		interval:      o.Interval,
		settleEnded:   o.SettleEnded,
		quietFor:      o.QuietFor,
		maxAge:        o.MaxAge,
		lease:         o.Lease,
		maxAttempts:   o.MaxAttempts,
		batch:         o.Batch,
		dailyCap:      o.DailyCap,
		minTurns:      o.MinTurns,
		minTools:      o.MinTools,
	}
	if m.log == nil {
		m.log = slog.Default()
	}
	if m.now == nil {
		m.now = time.Now
	}
	positive(&m.interval, defaultInterval)
	positive(&m.settleEnded, defaultSettleEnded)
	positive(&m.quietFor, defaultQuietFor)
	positive(&m.maxAge, defaultMaxAge)
	positive(&m.lease, defaultLease)
	positiveInt(&m.maxAttempts, defaultMaxAttempts)
	positiveInt(&m.batch, defaultBatch)
	positiveInt(&m.dailyCap, defaultDailyCap)
	// The substance thresholds may legitimately be set to zero, meaning "mirror
	// everything", so only a negative value is corrected. A deployment that
	// wants every one-turn pipeline call in a channel is entitled to ask.
	if m.minTurns < 0 {
		m.minTurns = defaultMinTurns
	}
	if m.minTools < 0 {
		m.minTools = defaultMinTools
	}
	return m, nil
}

func positive(d *time.Duration, def time.Duration) {
	if *d <= 0 {
		*d = def
	}
}

func positiveInt(n *int, def int) {
	if *n <= 0 {
		*n = def
	}
}

// Run posts on a schedule until ctx is cancelled.
//
// This is the caller the poster has to have. A mirror that is constructed,
// registered and never run is the same defect as a component with no
// composition root: it passes its own tests, it posts nothing, and the only
// symptom is that a feature somebody switched on does nothing at all.
//
// A timer rather than a ticker, reset after each pass, so the interval is a gap
// between passes rather than a period. A ticker that fires while a pass is
// still inside a Retry-After queues the tick and starts the next pass the
// instant this one ends, which is how a rate-limited poster turns into a
// continuous one.
func (m *Mirror) Run(ctx context.Context) {
	timer := time.NewTimer(m.interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		// Live threads advance first: a person watching a running session is
		// waiting; nobody is waiting for a digest.
		lp, lerr := m.livePass(ctx)
		switch {
		case lerr != nil && ctx.Err() != nil:
			m.log.Info("slack live pass stopped by shutdown", "pass", lp)
			return
		case lerr != nil:
			m.log.Error("slack live pass failed", "err", lerr, "pass", lp)
		case lp.Opened > 0 || lp.Replies > 0 || lp.Closed > 0 || lp.Failed > 0:
			m.log.Info("slack live", "pass", lp)
		}

		p, err := m.pass(ctx)
		switch {
		case err != nil && ctx.Err() != nil:
			// The shutdown cancelled the pass in flight. Whatever was posted is
			// recorded and whatever was claimed will be reclaimed after its
			// lease, so this is the drain working rather than a failure.
			m.log.Info("slack mirror stopped by shutdown", "pass", p)
			return
		case err != nil:
			// The counts travel with the error: a pass that posted nine messages
			// and then failed is a different event from one that failed on its
			// first query.
			m.log.Error("slack mirror pass failed", "err", err, "pass", p)
		case p.Posted > 0 || p.Failed > 0:
			// Silence when there was nothing to do, deliberately. This runs
			// every minute forever, and a line per minute saying "nothing" is a
			// log nobody can read and a bill nobody expected.
			m.log.Info("slack mirror", "pass", p)
		}
		timer.Reset(m.interval)
	}
}

// Pass is what one sweep did, and it is what Run logs.
type Pass struct {
	// Due is how many sessions the query offered, before claiming.
	Due int
	// Posted, Failed and Skipped account for every one of them. Skipped is a
	// key another pass or another instance already owned, which is the ordinary
	// outcome of two instances sweeping at once rather than an anomaly.
	Posted  int
	Failed  int
	Skipped int
	// Capped is how many were held back by somebody's daily ceiling.
	Capped int
}

// LogValue keeps a pass to one structured field rather than five.
func (p Pass) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Int("due", p.Due),
		slog.Int("posted", p.Posted),
		slog.Int("failed", p.Failed),
		slog.Int("skipped", p.Skipped),
		slog.Int("capped", p.Capped),
	)
}

// pass posts everything currently due, oldest first.
//
// Sequential rather than concurrent, and that is not laziness: chat.postMessage
// is rate-limited per channel, several people can share a channel, and the
// cheapest way to stay under a per-channel limit is to have one thing posting.
// At twenty-five messages a pass and a second between them, a full pass is under
// half a minute.
func (m *Mirror) pass(ctx context.Context) (Pass, error) {
	var p Pass
	now := m.now()
	due, err := eligible(ctx, m.db, window{
		endedBefore: now.Add(-m.settleEnded),
		quietBefore: now.Add(-m.quietFor),
		freshAfter:  now.Add(-m.maxAge),
		leaseBefore: now.Add(-m.lease),
		maxAttempts: m.maxAttempts,
		minTurns:    m.minTurns,
		minTools:    m.minTools,
		limit:       m.batch,
	})
	if err != nil {
		return p, err
	}
	p.Due = len(due)

	// budget is how many more messages each person may receive today, resolved
	// once per person per pass rather than once per message: it is a count over
	// their own history, and it cannot change during a pass except by this loop.
	budget := make(map[string]int, 4)
	for _, c := range due {
		if ctx.Err() != nil {
			// Reported as a partial pass rather than an error. Everything posted
			// so far is recorded, and the rest is still due next time.
			return p, ctx.Err()
		}

		left, known := budget[c.Email]
		if !known {
			sent, err := postedSince(ctx, m.db, c.Email, now.Add(-24*time.Hour))
			if err != nil {
				return p, err
			}
			left = m.dailyCap - sent
			budget[c.Email] = left
		}
		if left <= 0 {
			p.Capped++
			// The notice is claimed on the same table under a key of its own, so
			// it is sent once a day however many sessions are held back behind
			// it.
			m.notifyCapped(ctx, c, now)
			continue
		}

		posted, err := m.mirrorOne(ctx, c, now)
		switch {
		case posted:
			// Classified by what reached Slack rather than by whether the call
			// returned an error, because those are not the same question. A
			// message that was sent and could not then be recorded is a post,
			// and counting it as a failure would send whoever reads this log
			// looking for a message that did in fact arrive — and will arrive
			// again when the lease expires.
			p.Posted++
			budget[c.Email] = left - 1
			if err != nil {
				m.log.Error("slack mirror posted a session but could not record it, so it will be posted again",
					"session_id", c.SessionID, "email", c.Email, "err", err)
			}
		case err != nil:
			p.Failed++
			// One session's failure is not the pass's. The next session may be
			// somebody else's, in a different channel, and stopping here would
			// let one bad destination hold up everybody's.
			m.log.Warn("slack mirror could not post a session",
				"session_id", c.SessionID, "email", c.Email, "mode", string(c.Mode), "err", err)
		default:
			p.Skipped++
		}
	}
	return p, nil
}

// mirrorOne claims one session and posts it, reporting whether it did.
//
// The order is claim, then resolve, then post, then settle. Claiming first is
// what makes the concurrent case safe; it also means a session whose
// destination cannot be resolved consumes an attempt, which is intended — a DM
// to somebody Slack has never heard of should stop being retried every minute.
func (m *Mirror) mirrorOne(ctx context.Context, c candidate, now time.Time) (bool, error) {
	key := sessionKey(c.SessionID)
	claimed, err := claimKey(ctx, m.db, key, c.Email, c.SessionID, now, now.Add(-m.lease), m.maxAttempts)
	if err != nil {
		return false, err
	}
	if !claimed {
		return false, nil
	}

	channel, err := m.destination(ctx, c)
	if err != nil {
		return false, m.recordFailure(ctx, key, err)
	}

	ts, err := m.post(ctx, channel, render(c.Summary, m.publicURL, c.Mode))
	if err != nil {
		return false, m.recordFailure(ctx, key, err)
	}
	if err := markPosted(ctx, m.db, key, channel, ts, m.now()); err != nil {
		// The message is out. This is the narrow window the design accepts: the
		// key stays claimed, its lease expires, and a later pass posts it a
		// second time. Logged at error rather than swallowed, because a
		// duplicate in Slack with nothing in the log is unexplainable.
		return true, fmt.Errorf("slack: posted %s but could not record it: %w", key, err)
	}
	return true, nil
}

// recordFailure writes the reason down and returns it, so a caller cannot
// accidentally report a failure it did not record.
func (m *Mirror) recordFailure(ctx context.Context, key string, cause error) error {
	permanent := errors.Is(cause, ErrPermanent) || errors.Is(cause, ErrUserNotFound)
	if err := markFailed(ctx, m.db, key, cause, permanent, m.maxAttempts); err != nil {
		return fmt.Errorf("%w (and recording it failed: %w)", cause, err)
	}
	return cause
}

// destination resolves where this person's summary goes.
//
// Channel mode trusts the stored value, which the preference route validated
// when it was set. DM mode resolves the person's Slack account, caching the id
// the first time so the lookup is not repeated for every session.
func (m *Mirror) destination(ctx context.Context, c candidate) (string, error) {
	switch c.Mode {
	case ModeChannel:
		if c.Channel == "" {
			// Refused rather than silently redirected to a DM. A preference that
			// named a channel and posts somewhere else is worse than one that
			// posts nowhere.
			return "", fmt.Errorf("%w: channel mode with no channel", ErrPermanent)
		}
		return c.Channel, nil

	case ModeDM:
		userID := c.SlackUserID
		if userID == "" {
			resolved, err := m.slack.LookupUserByEmail(ctx, c.Email)
			if err != nil {
				return "", err
			}
			userID = resolved
			if err := cacheUserID(ctx, m.db, c.Email, userID); err != nil {
				// Not fatal: the message can still be posted, and the next pass
				// pays for another lookup. Logged so a cache that never sticks
				// is visible before it becomes a rate limit of its own.
				m.log.Warn("slack mirror could not cache a slack id", "email", c.Email, "err", err)
			}
		}
		return m.slack.OpenDM(ctx, userID)

	default:
		// Unreachable through the query, which filters mode <> 'off'. Here so
		// that a fourth mode added to the schema without being added here fails
		// loudly instead of posting somewhere unintended.
		return "", fmt.Errorf("%w: unknown mirror mode %q", ErrPermanent, string(c.Mode))
	}
}

// post sends the message, joining a public channel if that is what was missing.
//
// The join is reactive rather than speculative. Joining a channel is visible to
// everybody in it, so the bot appears there when somebody has actually pointed
// their mirror at it, not when the server started.
func (m *Mirror) post(ctx context.Context, channel, text string) (string, error) {
	ts, err := m.slack.PostMessage(ctx, channel, text)
	if err == nil {
		return ts, nil
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "not_in_channel" {
		return "", err
	}
	if joinErr := m.slack.JoinChannel(ctx, channel); joinErr != nil {
		// The original failure is what the operator needs; the join failure is
		// why it could not be fixed. Both, in that order.
		return "", fmt.Errorf("%w (and joining %s failed: %w)", err, channel, joinErr)
	}
	return m.slack.PostMessage(ctx, channel, text)
}

// notifyCapped posts the one message that explains a silence, at most once a
// day per person.
//
// Best effort by construction: this is a message about not sending messages, so
// a failure to send it is logged and nothing else. Returning an error here would
// let the notice fail a pass that was otherwise working.
func (m *Mirror) notifyCapped(ctx context.Context, c candidate, now time.Time) {
	key := capKey(c.Email, now)
	claimed, err := claimKey(ctx, m.db, key, c.Email, "", now, now.Add(-m.lease), m.maxAttempts)
	if err != nil || !claimed {
		return
	}
	channel, err := m.destination(ctx, c)
	if err != nil {
		_ = markFailed(ctx, m.db, key, err, errors.Is(err, ErrPermanent), m.maxAttempts)
		return
	}
	ts, err := m.post(ctx, channel, renderCapNotice(m.publicURL, m.dailyCap, c.Mode))
	if err != nil {
		_ = markFailed(ctx, m.db, key, err, errors.Is(err, ErrPermanent), m.maxAttempts)
		m.log.Warn("slack mirror could not post the daily-ceiling notice", "email", c.Email, "err", err)
		return
	}
	if err := markPosted(ctx, m.db, key, channel, ts, m.now()); err != nil {
		m.log.Error("slack mirror posted the daily-ceiling notice but could not record it",
			"email", c.Email, "err", err)
	}
	m.log.Info("slack mirror reached its daily ceiling for one person",
		"email", c.Email, "cap", m.dailyCap)
}
