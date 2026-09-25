package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultBaseURL is Slack's Web API. It is a field on Client rather than a
// constant in the call path so the tests can point the whole package at a fake
// Slack and never at this one.
const DefaultBaseURL = "https://slack.com/api"

const (
	// postInterval is the pace chat.postMessage is held to.
	//
	// Slack documents that method as roughly one message per second per channel
	// with short bursts tolerated, and answers 429 when it disagrees with you.
	// Pacing here is not a substitute for handling the 429 — the limit is
	// per-channel and applies across every app in the workspace, so ours is not
	// the only traffic counted — it is what keeps this poster from being the
	// reason the limit is reached. Slightly over a second, because a limiter
	// tuned exactly to the published rate spends its life discovering that the
	// published rate is approximate.
	postInterval = 1100 * time.Millisecond

	// maxAttempts bounds one logical call, including its retries after a 429.
	// Beyond this the message goes back to the outbox and waits for a later
	// pass, which is a slower retry against a service that has just said it
	// wants less traffic.
	maxAttempts = 3

	// retryAfterCap bounds how long a single call will sit inside Retry-After.
	// Slack occasionally names a very long window; honouring one would hold the
	// whole pass, and the pass is the only thing draining the outbox. Past the
	// cap the message is deferred instead, which is the same wait without a
	// goroutine spent on it.
	retryAfterCap = 30 * time.Second

	// retryAfterDefault applies when Slack rate-limits us without saying for how
	// long, which is what an ok:false "ratelimited" body carries: HTTP 200, no
	// header, no number.
	retryAfterDefault = time.Second

	// requestTimeout bounds one HTTP round trip to Slack. The mirror runs on a
	// timer, so a call that hangs holds a pass rather than a request, but a pass
	// that never ends is a mirror that stops after its first bad minute.
	requestTimeout = 10 * time.Second
)

// ErrPermanent marks a Slack refusal that retrying cannot fix.
//
// The distinction is the whole reason this package classifies errors at all. A
// message to a channel that does not exist will never be accepted, and retrying
// it every minute forever is how a small mistake becomes the thing that gets the
// bot rate-limited. A 429 or a 503 is the opposite: the same message will be
// accepted shortly, and giving up on it loses somebody's summary.
var ErrPermanent = errors.New("slack: permanent failure")

// APIError is Slack answering ok:false, or answering with a status that is not
// a 200.
//
// Slack reports failure inside a 200 body as a matter of course, so a client
// that only checks the status code reports every one of these as a success and
// posts nothing at all. Code is Slack's own string, kept verbatim: it is the
// only part of this that is worth putting in a log line, and rewording it would
// make the log un-greppable against Slack's own documentation.
type APIError struct {
	Method string
	Code   string
	Status int
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("slack: %s: %s", e.Method, e.Code)
	}
	return fmt.Sprintf("slack: %s: http %d", e.Method, e.Status)
}

// Is reports ErrPermanent for the refusals that will never become acceptances.
//
// The transient set is named explicitly and everything else is permanent, which
// is deliberately the conservative direction for a retry decision: mistaking a
// transient failure for a permanent one drops one message, and mistaking a
// permanent one for transient aims a retry loop at a service that has already
// said no. Both are bounded by the outbox's attempt count in any case; this only
// decides how quickly we stop.
func (e *APIError) Is(target error) bool {
	if target != ErrPermanent {
		return false
	}
	return !e.transient()
}

func (e *APIError) transient() bool {
	if e.Status >= 500 || e.Status == http.StatusTooManyRequests {
		return true
	}
	switch e.Code {
	case "ratelimited", "rate_limited", "service_unavailable", "internal_error",
		"fatal_error", "request_timeout", "":
		return true
	}
	return false
}

// ErrUserNotFound means Slack has nobody with that email address.
//
// Separated from the other permanent failures because it is the only one a
// person can act on themselves, and because it is the one that happens: a
// Workspace account created under a personal address, or under the other of the
// company's two domains. The preference endpoint reports it to the person who
// just asked for a mirror, at the moment they asked, rather than leaving them to
// wonder why nothing arrives.
var ErrUserNotFound = errors.New("slack: no Slack account carries that email address")

// Client is the four Slack methods this package needs.
//
// It is a concrete type rather than an interface because the interesting
// behaviour is inside it — the pacing, the 429 handling, the fact that ok:false
// inside a 200 is a failure — and a mocked interface would test none of that.
// Every test in this package points BaseURL at an httptest server and exercises
// this code for real.
type Client struct {
	token string
	http  *http.Client
	base  string

	// pace serialises chat.postMessage and holds it to postInterval. A mutex
	// rather than a token bucket: at this volume the queue is never more than a
	// message deep, and a bucket would be a second thing to reason about at the
	// moment somebody is asking why a message was late.
	pace     sync.Mutex
	lastPost time.Time

	now   func() time.Time
	sleep func(context.Context, time.Duration) error
}

// ClientOptions configure a Client. Only Token is required.
type ClientOptions struct {
	// Token is the bot token, xoxb-. It authorises everything this package does
	// and is never logged: see Mirror's logger, which reports what was posted
	// and where, never with what.
	Token string

	// BaseURL replaces Slack's own, for tests. Empty means the real one, which
	// is what makes "the tests never post to the workspace" a property of the
	// default rather than of every test remembering to override it.
	BaseURL string

	// HTTPClient is injectable so a deployment can supply its own transport.
	HTTPClient *http.Client

	// Now and Sleep exist so the pacing and the Retry-After handling can be
	// tested in milliseconds rather than in the seconds they take in production.
	Now   func() time.Time
	Sleep func(context.Context, time.Duration) error
}

// NewClient builds a Client. A missing token is refused here rather than at the
// first post, because a mirror that starts, polls, claims a session and then
// discovers it has no credential has already consumed the claim.
func NewClient(o ClientOptions) (*Client, error) {
	if strings.TrimSpace(o.Token) == "" {
		return nil, errors.New("slack: a bot token is required")
	}
	c := &Client{
		token: strings.TrimSpace(o.Token),
		http:  o.HTTPClient,
		base:  strings.TrimSuffix(strings.TrimSpace(o.BaseURL), "/"),
		now:   o.Now,
		sleep: o.Sleep,
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: requestTimeout}
	}
	if c.base == "" {
		c.base = DefaultBaseURL
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.sleep == nil {
		c.sleep = sleepCtx
	}
	return c, nil
}

// sleepCtx waits, and gives up if the server is shutting down. A drain that has
// to wait out a Retry-After is a drain that overruns Cloud Run's grace.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// PostMessage posts text to a channel or DM and reports Slack's message id.
//
// The pacing is held for the whole call including its retries, so a 429 slows
// this poster down rather than letting a second goroutine step over the top of
// the message that just triggered it.
func (c *Client) PostMessage(ctx context.Context, channel, text string) (string, error) {
	c.pace.Lock()
	defer c.pace.Unlock()

	if !c.lastPost.IsZero() {
		if wait := postInterval - c.now().Sub(c.lastPost); wait > 0 {
			if err := c.sleep(ctx, wait); err != nil {
				return "", err
			}
		}
	}
	defer func() { c.lastPost = c.now() }()

	var out struct {
		TS string `json:"ts"`
	}
	err := c.call(ctx, "chat.postMessage", url.Values{
		"channel": {channel},
		"text":    {text},
		// Slack renders a link inside a text field only when mrkdwn is on, and
		// the link is the entire point of the message.
		"mrkdwn": {"true"},
		// A summary carries no URLs of its own except the dashboard link, and an
		// unfurl of that link would put the dashboard's own page title into the
		// channel. Off, so what is posted is exactly what this package composed.
		"unfurl_links": {"false"},
		"unfurl_media": {"false"},
	}, &out)
	if err != nil {
		return "", err
	}
	return out.TS, nil
}

// PostMessageBlocks posts a message with Block Kit blocks beside the text
// fallback. Same pacing as every other post.
func (c *Client) PostMessageBlocks(ctx context.Context, channel, text, blocksJSON string) (string, error) {
	c.pace.Lock()
	defer c.pace.Unlock()
	if !c.lastPost.IsZero() {
		if wait := postInterval - c.now().Sub(c.lastPost); wait > 0 {
			if err := c.sleep(ctx, wait); err != nil {
				return "", err
			}
		}
	}
	defer func() { c.lastPost = c.now() }()

	var out struct {
		TS string `json:"ts"`
	}
	err := c.call(ctx, "chat.postMessage", url.Values{
		"channel":      {channel},
		"text":         {text},
		"blocks":       {blocksJSON},
		"mrkdwn":       {"true"},
		"unfurl_links": {"false"},
		"unfurl_media": {"false"},
	}, &out)
	if err != nil {
		return "", err
	}
	return out.TS, nil
}

// PostThreadReply posts into an existing thread. The same pacing, mrkdwn and
// unfurl decisions as PostMessage, because a reply is a message; the only
// difference is the thread it lands in.
func (c *Client) PostThreadReply(ctx context.Context, channel, threadTS, text string) (string, error) {
	c.pace.Lock()
	defer c.pace.Unlock()
	if !c.lastPost.IsZero() {
		if wait := postInterval - c.now().Sub(c.lastPost); wait > 0 {
			if err := c.sleep(ctx, wait); err != nil {
				return "", err
			}
		}
	}
	defer func() { c.lastPost = c.now() }()

	var out struct {
		TS string `json:"ts"`
	}
	err := c.call(ctx, "chat.postMessage", url.Values{
		"channel":      {channel},
		"thread_ts":    {threadTS},
		"text":         {text},
		"mrkdwn":       {"true"},
		"unfurl_links": {"false"},
		"unfurl_media": {"false"},
	}, &out)
	if err != nil {
		return "", err
	}
	return out.TS, nil
}

// UpdateMessage edits a message in place — the live root header. Paced with
// the same mutex as posting: chat.update draws from the same per-channel
// rate budget, and an update storm can mute the bot as surely as a post
// storm.
func (c *Client) UpdateMessage(ctx context.Context, channel, ts, text string) error {
	c.pace.Lock()
	defer c.pace.Unlock()
	if !c.lastPost.IsZero() {
		if wait := postInterval - c.now().Sub(c.lastPost); wait > 0 {
			if err := c.sleep(ctx, wait); err != nil {
				return err
			}
		}
	}
	defer func() { c.lastPost = c.now() }()

	return c.call(ctx, "chat.update", url.Values{
		"channel": {channel},
		"ts":      {ts},
		"text":    {text},
		"mrkdwn":  {"true"},
	}, &struct{}{})
}

// UpdateMessageBlocks edits a message in place keeping its interactive
// blocks. chat.update REPLACES content wholesale: an update that sends only
// text silently strips the buttons the message was posted with, which is how
// every live root lost its Stop button on the first heartbeat edit.
func (c *Client) UpdateMessageBlocks(ctx context.Context, channel, ts, text, blocksJSON string) error {
	c.pace.Lock()
	defer c.pace.Unlock()
	if !c.lastPost.IsZero() {
		if wait := postInterval - c.now().Sub(c.lastPost); wait > 0 {
			if err := c.sleep(ctx, wait); err != nil {
				return err
			}
		}
	}
	defer func() { c.lastPost = c.now() }()

	return c.call(ctx, "chat.update", url.Values{
		"channel": {channel},
		"ts":      {ts},
		"text":    {text},
		"blocks":  {blocksJSON},
	}, nil)
}

// JoinChannel joins a public channel, which is what chat.postMessage requires
// before it will accept a message for one.
//
// Called only in response to not_in_channel, never speculatively: joining is
// visible to everybody in the channel, and a mirror that joined channels at
// startup would announce itself in places nobody had finished configuring.
func (c *Client) JoinChannel(ctx context.Context, channel string) error {
	return c.call(ctx, "conversations.join", url.Values{"channel": {channel}}, nil)
}

// LookupUserByEmail resolves a company address to a Slack user id.
func (c *Client) LookupUserByEmail(ctx context.Context, email string) (string, error) {
	var out struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	err := c.call(ctx, "users.lookupByEmail", url.Values{"email": {email}}, &out)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && (apiErr.Code == "users_not_found" || apiErr.Code == "user_not_found") {
			return "", fmt.Errorf("%w: %s", ErrUserNotFound, email)
		}
		return "", err
	}
	if out.User.ID == "" {
		return "", fmt.Errorf("%w: %s", ErrUserNotFound, email)
	}
	return out.User.ID, nil
}

// OpenDM opens the conversation between the bot and one person and reports its
// channel id.
//
// Called on every post rather than cached, because the id it returns is stable
// and cheap to re-derive while a cache of it is one more thing that can be
// stale after somebody is deactivated and reinstated. It is the DM between the
// bot and that person, so nobody else can read it: this is the only destination
// in this package that carries no risk of an audience the person did not pick.
func (c *Client) OpenDM(ctx context.Context, userID string) (string, error) {
	var out struct {
		Channel struct {
			ID string `json:"id"`
		} `json:"channel"`
	}
	if err := c.call(ctx, "conversations.open", url.Values{"users": {userID}}, &out); err != nil {
		return "", err
	}
	if out.Channel.ID == "" {
		return "", &APIError{Method: "conversations.open", Code: "no_channel"}
	}
	return out.Channel.ID, nil
}

// call performs one Slack Web API method, retrying only a rate limit.
//
// Form encoding rather than JSON, because it is what every one of these methods
// has always accepted and because it puts the token in a header rather than in
// a body somewhere that could be logged by a proxy.
func (c *Client) call(ctx context.Context, method string, form url.Values, out any) error {
	for attempt := 1; ; attempt++ {
		wait, err := c.attempt(ctx, method, form, out)
		switch {
		case err == nil:
			return nil
		case wait <= 0:
			// Not a rate limit, so repeating it would only repeat the answer.
			return err
		case attempt >= maxAttempts:
			// Reported as what it is rather than as a generic failure: the
			// outbox retries on a later pass, and a log line saying "rate
			// limited" three times running is the signal that the pace is wrong.
			return fmt.Errorf("slack: %s rate limited after %d attempts: %w", method, attempt, err)
		}
		if err := c.sleep(ctx, wait); err != nil {
			return err
		}
	}
}

// attempt makes one request and reports how long to wait before repeating it.
// A zero wait means the error is not a rate limit and repeating it is pointless.
func (c *Client) attempt(ctx context.Context, method string, form url.Values, out any) (time.Duration, error) {
	body := strings.NewReader(form.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/"+method, body)
	if err != nil {
		return 0, fmt.Errorf("slack: build %s request: %w", method, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")

	resp, err := c.http.Do(req)
	if err != nil {
		// A transport failure is not a rate limit and is not permanent either:
		// the outbox retries it on the next pass.
		return 0, fmt.Errorf("slack: call %s: %w", method, err)
	}
	defer func() {
		// Drained before closing so the connection returns to the pool rather
		// than being torn down, which matters on a poster that makes one call a
		// minute and would otherwise renegotiate TLS every time.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode == http.StatusTooManyRequests {
		return retryAfter(resp.Header.Get("Retry-After")),
			&APIError{Method: method, Code: "ratelimited", Status: resp.StatusCode}
	}
	if resp.StatusCode != http.StatusOK {
		return 0, &APIError{Method: method, Status: resp.StatusCode}
	}

	// Slack reports failure inside a 200. Decoding into the envelope first is
	// what makes ok:false a failure here rather than an empty success upstream.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, fmt.Errorf("slack: read %s response: %w", method, err)
	}
	var env struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return 0, fmt.Errorf("slack: decode %s response: %w", method, err)
	}
	if !env.OK {
		apiErr := &APIError{Method: method, Code: env.Error, Status: resp.StatusCode}
		if env.Error == "ratelimited" || env.Error == "rate_limited" {
			// No header on this shape, so the wait is ours to choose.
			return retryAfterDefault, apiErr
		}
		return 0, apiErr
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return 0, fmt.Errorf("slack: decode %s payload: %w", method, err)
		}
	}
	return 0, nil
}

// retryAfter reads the header Slack sends with a 429, bounded.
//
// An unparseable or absent value becomes the default rather than zero. Zero
// would mean "not a rate limit" to the caller, which would turn the one error
// that must be retried into the one that is not.
func retryAfter(h string) time.Duration {
	secs, err := strconv.Atoi(strings.TrimSpace(h))
	if err != nil || secs < 0 {
		return retryAfterDefault
	}
	d := time.Duration(secs) * time.Second
	if d <= 0 {
		return retryAfterDefault
	}
	if d > retryAfterCap {
		return retryAfterCap
	}
	return d
}

// Channel is one workspace channel the bot can see.
type Channel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ListChannels reads the workspace's public channels, paginating to the end.
// The bot token carries channels:read; private channels need groups:read and
// are deliberately absent — a destination the bot cannot see is entered by id
// and fails visibly at post time rather than silently missing from a picker.
func (c *Client) ListChannels(ctx context.Context) ([]Channel, error) {
	var out []Channel
	cursor := ""
	for {
		var resp struct {
			Channels []struct {
				ID         string `json:"id"`
				Name       string `json:"name"`
				IsArchived bool   `json:"is_archived"`
			} `json:"channels"`
			Meta struct {
				NextCursor string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		form := url.Values{
			"types":            {"public_channel"},
			"exclude_archived": {"true"},
			"limit":            {"1000"},
		}
		if cursor != "" {
			form.Set("cursor", cursor)
		}
		if err := c.call(ctx, "conversations.list", form, &resp); err != nil {
			return nil, err
		}
		for _, ch := range resp.Channels {
			if !ch.IsArchived {
				out = append(out, Channel{ID: ch.ID, Name: ch.Name})
			}
		}
		cursor = resp.Meta.NextCursor
		if cursor == "" {
			return out, nil
		}
	}
}

// User is one workspace member the person picker offers.
type User struct {
	ID string `json:"id"`
	// Handle is the @name, the workspace-unique spelling the picker submits.
	Handle string `json:"handle"`
	// RealName is what the picker displays beside the handle.
	RealName string `json:"real_name"`
}

// ListUsers reads the workspace's people, paginating to the end. Bots,
// deleted accounts and Slackbot are dropped here rather than at render time,
// because no destination picker should ever offer them: a group whose DM is a
// bot delivers to nobody.
func (c *Client) ListUsers(ctx context.Context) ([]User, error) {
	var out []User
	cursor := ""
	for {
		var resp struct {
			Members []struct {
				ID      string `json:"id"`
				Name    string `json:"name"`
				Deleted bool   `json:"deleted"`
				IsBot   bool   `json:"is_bot"`
				Profile struct {
					RealName string `json:"real_name"`
				} `json:"profile"`
			} `json:"members"`
			Meta struct {
				NextCursor string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		form := url.Values{"limit": {"200"}}
		if cursor != "" {
			form.Set("cursor", cursor)
		}
		if err := c.call(ctx, "users.list", form, &resp); err != nil {
			return nil, err
		}
		for _, u := range resp.Members {
			if u.Deleted || u.IsBot || u.ID == "USLACKBOT" {
				continue
			}
			out = append(out, User{ID: u.ID, Handle: u.Name, RealName: u.Profile.RealName})
		}
		cursor = resp.Meta.NextCursor
		if cursor == "" {
			return out, nil
		}
	}
}
