package slack

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The live thread's whole arc against the fake Slack: a root once the session
// shows substance, a reply per turn and per milestone — each exactly once
// however many passes run — and the digest summary as the closing reply once
// the session settles.
func TestLiveThreadOpensRepliesAndCloses(t *testing.T) {
	fs := newFakeSlack(t)
	db := newFakeDB()
	// The mirror's clock is pinned by testMirror; fixtures share it so the
	// settle arithmetic means what it says.
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)

	c := liveCandidate{
		SessionID: "s-live", Email: "ana@example.org", Repo: "backend",
		StartedAt: now.Add(-10 * time.Minute), UpdatedAt: now,
		UserTurns: 3, ToolCalls: 40, CostUSD: 1.5,
		Request: "on", Mode: ModeChannel, Channel: "C123",
	}
	db.live = []liveCandidate{c}
	db.turns["s-live"] = []liveTurn{
		{Seq: 1, OccurredAt: now.Add(-9 * time.Minute), Text: "fix the ingest lag\nand more",
			Reply: "Lag was the settle window; fixed and deployed."},
		{Seq: 5, OccurredAt: now.Add(-5 * time.Minute), Text: "now ship it"},
	}
	db.miles["s-live"] = []liveMilestone{{Kind: "pr", About: "https://github.com/x/y/pull/9"}}

	m := testMirror(t, db, fs.client(t), func(o *Options) {
		o.MinTurns, o.MinTools = 2, 10
	})
	ctx := context.Background()

	if _, err := m.livePass(ctx); err != nil {
		t.Fatalf("first live pass: %v", err)
	}
	posts := fs.posts()
	// Root, ONE completed exchange (the in-flight turn waits for its answer),
	// and the PR milestone. Legibility over immediacy, by owner decree.
	if len(posts) != 3 {
		t.Fatalf("first pass posted %d messages, want root + 1 exchange + 1 milestone", len(posts))
	}
	if !strings.Contains(posts[0].Text, "live session") || posts[0].Channel != "C123" {
		t.Errorf("root = %+v", posts[0])
	}
	// The exchange reads as a conversation: both speakers, prompt flattened.
	if !strings.Contains(posts[1].Text, "*ana:* fix the ingest lag and more") ||
		!strings.Contains(posts[1].Text, "*agent:* Lag was the settle window") {
		t.Errorf("exchange must carry both speakers: %q", posts[1].Text)
	}
	if strings.Contains(posts[1].Text, "now ship it") {
		t.Errorf("the in-flight turn leaked before its answer existed: %q", posts[1].Text)
	}
	if !strings.Contains(posts[2].Text, "pull/9") {
		t.Errorf("milestone reply: %q", posts[2].Text)
	}

	// A second pass re-says nothing: every key is settled in the ledger.
	if _, err := m.livePass(ctx); err != nil {
		t.Fatalf("second live pass: %v", err)
	}
	if extra := len(fs.posts()) - 3; extra != 0 {
		t.Fatalf("a repeated pass posted %d more messages", extra)
	}

	// The session announces its end and settles: one closing summary, and the
	// thread is done — a third pass after that touches nothing.
	db.mu.Lock()
	db.live[0].Ended = true
	db.live[0].UpdatedAt = now.Add(-time.Hour)
	db.mu.Unlock()
	if _, err := m.livePass(ctx); err != nil {
		t.Fatalf("closing pass: %v", err)
	}
	posts = fs.posts()
	if len(posts) != 5 {
		t.Fatalf("closing pass posted %d total, want 5", len(posts))
	}
	if !strings.Contains(posts[4].Text, "ana@example.org") {
		t.Errorf("the close is the digest summary: %q", posts[4].Text)
	}
	if _, err := m.livePass(ctx); err != nil {
		t.Fatalf("post-close pass: %v", err)
	}
	if len(fs.posts()) != 5 {
		t.Fatal("a closed thread was posted to again")
	}
}

// A request naming a channel is honoured as typed; the standing preference
// still supplies consent, but not the destination.
func TestLiveRequestNamingAChannelWins(t *testing.T) {
	fs := newFakeSlack(t)
	db := newFakeDB()
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	db.live = []liveCandidate{{
		SessionID: "s-ch", Email: "ana@example.org",
		StartedAt: now.Add(-time.Minute), UpdatedAt: now,
		UserTurns: 5, ToolCalls: 50,
		Request: "C0TEST99", Mode: ModeDM, SlackUserID: "U1",
	}}
	m := testMirror(t, db, fs.client(t), nil)
	if _, err := m.livePass(context.Background()); err != nil {
		t.Fatalf("live pass: %v", err)
	}
	posts := fs.posts()
	if len(posts) == 0 || posts[0].Channel != "C0TEST99" {
		t.Fatalf("root went to %+v, want the channel the launch named", posts)
	}
}

// A trivial session that asked — a stale exported env var on an hourly ping —
// opens nothing until it shows substance.
func TestLiveTrivialSessionOpensNoThread(t *testing.T) {
	fs := newFakeSlack(t)
	db := newFakeDB()
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	db.live = []liveCandidate{{
		SessionID: "s-ping", Email: "ana@example.org",
		StartedAt: now, UpdatedAt: now,
		UserTurns: 1, ToolCalls: 0,
		Request: "on", Mode: ModeChannel, Channel: "C123",
	}}
	m := testMirror(t, db, fs.client(t), func(o *Options) {
		o.MinTurns, o.MinTools = 2, 10
	})
	if _, err := m.livePass(context.Background()); err != nil {
		t.Fatalf("live pass: %v", err)
	}
	if n := len(fs.posts()); n != 0 {
		t.Fatalf("a trivial session opened a thread (%d posts)", n)
	}
}

// Fan-out: one session attached to two groups narrates two independent
// threads — separate roots, separate replies, separate closes — because a
// thread is a (session, group) pair, not a session.
// TestInteractiveRootKeepsItsButtonThroughEdits pins the chat.update trap:
// updates replace content wholesale, so the heartbeat edit must re-send the
// Stop button or the root silently loses it — which is exactly how the first
// live thread shipped.
func TestInteractiveRootKeepsItsButtonThroughEdits(t *testing.T) {
	fs := newFakeSlack(t)
	db := newFakeDB()
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	db.groupActs = []groupActivation{{
		liveCandidate: liveCandidate{
			SessionID: "s-live", Email: "ana@example.org", Repo: "backend",
			StartedAt: now.Add(-30 * time.Minute), UpdatedAt: now,
			UserTurns: 5, ToolCalls: 60,
		},
		GroupID: 3, GroupName: "eng", Destination: "C0ENG",
	}}
	db.turns["s-live"] = []liveTurn{
		{Seq: 1, OccurredAt: now.Add(-20 * time.Minute), Text: "go", Reply: "done"},
		{Seq: 4, OccurredAt: now.Add(-2 * time.Minute), Text: "next"},
	}

	m := testMirror(t, db, fs.client(t), func(o *Options) {
		o.MinTurns, o.MinTools = 2, 10
		o.SigningSecret = "shhh"
	})
	if _, err := m.livePass(context.Background()); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	// Second pass: the root exists, the session is still fresh, so the pass
	// edits the header in place.
	if _, err := m.livePass(context.Background()); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	var sawRootPost, sawUpdate bool
	for _, c := range fs.snapshot() {
		switch c.Method {
		case "chat.postMessage":
			if strings.Contains(c.Blocks, "stop_mirror") {
				sawRootPost = true
			}
		case "chat.update":
			sawUpdate = true
			if !strings.Contains(c.Blocks, "stop_mirror") {
				t.Fatalf("a header edit dropped the Stop button; blocks = %q", c.Blocks)
			}
		}
	}
	if !sawRootPost || !sawUpdate {
		t.Fatalf("expected an interactive root post and an edit; post=%v update=%v", sawRootPost, sawUpdate)
	}
}

func TestGroupFanOutIsPerPair(t *testing.T) {
	fs := newFakeSlack(t)
	db := newFakeDB()
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	base := liveCandidate{
		SessionID: "s-fan", Email: "ana@example.org", Repo: "backend",
		StartedAt: now.Add(-10 * time.Minute), UpdatedAt: now,
		UserTurns: 3, ToolCalls: 40,
	}
	db.groupActs = []groupActivation{
		{liveCandidate: base, GroupID: 1, GroupName: "eng", Destination: "C0ENG"},
		{liveCandidate: base, GroupID: 2, GroupName: "mine", Destination: "C0MINE"},
	}
	db.turns["s-fan"] = []liveTurn{
		{Seq: 1, OccurredAt: now.Add(-9 * time.Minute), Text: "go", Reply: "done"},
		{Seq: 4, OccurredAt: now.Add(-2 * time.Minute), Text: "next"},
	}

	m := testMirror(t, db, fs.client(t), func(o *Options) { o.MinTurns, o.MinTools = 2, 10 })
	if _, err := m.livePass(context.Background()); err != nil {
		t.Fatalf("live pass: %v", err)
	}
	byChan := map[string]int{}
	for _, c := range fs.posts() {
		byChan[c.Channel]++
	}
	if byChan["C0ENG"] != 2 || byChan["C0MINE"] != 2 {
		t.Fatalf("fan-out posted %v, want root+turn in each destination", byChan)
	}
	// Idempotent across both pairs.
	if _, err := m.livePass(context.Background()); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if len(fs.posts()) != 4 {
		t.Fatalf("repeat pass changed the count to %d", len(fs.posts()))
	}
}

// The milestone read filters on an attachment horizon it must actually bind.
//
// This is a regression test with an incident behind it: the statement filtered
// on $4 and bound three arguments, so Postgres refused it on every pass and the
// mirror stalled after the root message — silently, in a WARN, for as long as
// it took somebody to read the logs. Calling the reader directly is what makes
// the failure legible; driving it through a whole pass only shows a missing
// post, which is what a milestone-free session looks like too.
func TestLiveMilestoneReadBindsItsHorizon(t *testing.T) {
	db := newFakeDB()
	db.miles["s-m"] = []liveMilestone{{Kind: "pr", About: "https://github.com/x/y/pull/1"}}
	after := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)

	got, err := liveMilestones(context.Background(), db, "s-m", threadPrefix("s-m"), after, 10)
	if err != nil {
		t.Fatalf("read milestones: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("read %d milestones, want 1", len(got))
	}

	args := db.argsFor("FROM links l")
	if len(args) != 4 {
		t.Fatalf("the milestone read bound %d arguments; the statement references $4", len(args))
	}
	if args[3] != any(after) {
		t.Errorf("fourth argument = %v, want the horizon %v", args[3], after)
	}
}
