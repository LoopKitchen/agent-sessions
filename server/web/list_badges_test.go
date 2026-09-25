package web

import (
	"strings"
	"testing"
	"time"
)

// TestSessionListReadsFacetsOnceForThePageAndBadgesEveryState: the list
// issues one list query and one facets query per page, and every badge the
// lattice can carry renders from the facet, the Turns column is human_turns,
// and the first answer is the snippet.
func TestSessionListReadsFacetsOnceForThePageAndBadgesEveryState(t *testing.T) {
	f := newFake()
	start := fixedNow.Add(-2 * time.Hour)
	for i, spec := range []struct {
		id    string
		typ   string
		facet SessionFacet
	}{
		{"s-plain", "user", SessionFacet{Type: "user", HeadState: "complete", TitleSource: "human", HumanTurns: 7, FirstAnswer: "The tests pass now, all forty of them."}},
		{"s-head", "user", SessionFacet{Type: "user", HeadState: "truncated_window", TitleSource: "command", HumanTurns: 3}},
		{"s-lost", "user", SessionFacet{Type: "user", HeadState: "capture_loss", TitleSource: "human", HumanTurns: 2, CaptureLossDrops: 9}},
		{"s-cont", "user", SessionFacet{Type: "user", HeadState: "complete", TitleSource: "human", LineageSource: "fork_uuid", ParentSessionID: "s-plain", HumanTurns: 1}},
		{"s-auto", "automation", SessionFacet{Type: "automation", HeadState: "complete", TitleSource: "automation_template", HumanTurns: 0}},
		{"s-int", "internal", SessionFacet{Type: "internal", HeadState: "complete", TitleSource: "human", HumanTurns: 1}},
		{"s-empty", "empty", SessionFacet{Type: "empty", EmptyKind: "aborted", HeadState: "unknown", TitleSource: "none"}},
	} {
		f.sessions[spec.id] = SessionDetail{Session: Session{
			ID: spec.id, Email: owner.Email, Type: spec.typ, Source: "claude_code", Repo: "acme/api",
			StartedAt: start.Add(time.Duration(i) * time.Minute), EndedAt: start.Add(time.Duration(i+1) * time.Minute), Ended: true,
			UserTurns: 99, FirstPrompt: "prompt for " + spec.id,
		}, Via: "own"}
		f.facets[spec.id] = spec.facet
	}
	s := newServer(t, f, owner)

	rec := get(t, s, "/sessions")
	body := rec.Body.String()
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if f.facetReads != 1 {
		t.Fatalf("the list issued %d facets reads for one page, want exactly one", f.facetReads)
	}
	if f.lastQuery.Types != nil {
		t.Errorf("the default list sent types %v, want nil for the store default", f.lastQuery.Types)
	}
	for _, want := range []string{
		`title="Capture begins after the session did; the head was not imported">head</span>`,
		`>capture loss</span>`,
		`title="Continues an earlier session (proven by fork_uuid)">continued</span>`,
		`>auto</span>`,
		`>internal</span>`,
		`>empty (aborted)</span>`,
		`title="Title derived from a command, not typed by a person">command</span>`,
		`title="Title derived from a template, not typed by a person">template</span>`,
		`<span class="answer-snippet">The tests pass now, all forty of them.</span>`,
		`title="Turns a person opened">7</td>`,
		`title="Turns a person opened">3</td>`,
		"people and automations",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s", want)
		}
	}
	// The Turns column is human_turns, never the additive user_turns.
	if strings.Contains(body, `title="Turns a person opened">99</td>`) {
		t.Error("the Turns column shows user_turns")
	}
	// The continued badge is earned by lineage_source alone: a session that
	// merely has a parent id shows nothing.
	f.sessions["s-plain"] = SessionDetail{Session: Session{ID: "s-plain", Email: owner.Email, Type: "user", ParentID: "ghost", StartedAt: start, Ended: true}}
	f.facets["s-plain"] = SessionFacet{Type: "user", HeadState: "complete", TitleSource: "human"}
	if body := get(t, s, "/sessions").Body.String(); strings.Count(body, ">continued</span>") != 1 {
		t.Errorf("continued badges = %d, want only the fork's", strings.Count(body, ">continued</span>"))
	}
	// A facets failure costs the badges and not the list.
	f.facets = nil
	if rec := get(t, s, "/sessions"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "s-plain") {
		t.Error("the list did not render when the facets were empty")
	}
}

// TestSessionListDefaultIsTheStoreDefaultWithChipsForTheHiddenClasses: no
// types parameter means the store's own view; the chips offer internal and
// empty; toggling back to the default clears the parameter.
func TestSessionListDefaultIsTheStoreDefaultWithChipsForTheHiddenClasses(t *testing.T) {
	f := newFake()
	f.seedSession("s1", owner.Email)
	s := newServer(t, f, owner)
	body := get(t, s, "/sessions").Body.String()
	if f.lastQuery.Types != nil {
		t.Fatalf("default list types = %v, want nil (the store default)", f.lastQuery.Types)
	}
	for _, want := range []string{
		`href="/sessions?types=user%2Cinternal%2Cautomation"`,
		`href="/sessions?types=user%2Cautomation%2Cempty"`,
		`class="pick on" href="/sessions?types=automation">user-initiated`,
		`class="pick on" href="/sessions?types=user">automation`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing chip link %s", want)
		}
	}
	// Toggling internal on and off again lands on the default with no
	// parameter, and the search follows the list's selection.
	get(t, s, "/sessions?types=user,internal,automation&q=flaky")
	if got := f.lastQuery.Types; len(got) != 3 {
		t.Errorf("explicit types reached the query as %v", got)
	}
	if n := len(f.searches); n == 0 || len(f.searches[n-1].Types) != 3 {
		t.Error("the search did not follow the list's selection")
	}
	body = get(t, s, "/sessions?types=user,internal,automation").Body.String()
	if !strings.Contains(body, `class="pick on" href="/sessions">internal`) {
		t.Error("toggling internal off does not return to the clean default address")
	}
}

// TestSearchExcludesHarnessMessagesUnlessToggled: the toggle rides the URL
// and the form, reaches the store as IncludeHarness, and hits are labelled
// by kind.
func TestSearchExcludesHarnessMessagesUnlessToggled(t *testing.T) {
	f := newFake()
	f.seedSession("s1", owner.Email)
	f.hits = []SearchHit{
		{Session: f.sessions["s1"].Session, EventID: "ev2", Seq: 2, Role: "user", Kind: "slash_command", OccurredAt: fixedNow, Text: "/git flaky"},
	}
	s := newServer(t, f, owner)
	body := get(t, s, "/sessions?q=flaky").Body.String()
	if n := len(f.searches); n == 0 || f.searches[n-1].IncludeHarness {
		t.Fatal("the default search included harness messages")
	}
	if !strings.Contains(body, `href="/sessions?harness=1&amp;q=flaky"`) || strings.Contains(body, `class="pick on" href="/sessions?harness=1`) {
		t.Errorf("no off-state toggle link: %s", body[strings.Index(body, "include harness"):min(len(body), strings.Index(body, "include harness")+40)])
	}
	if !strings.Contains(body, `<span class="tag">command</span>`) {
		t.Error("the hit is not labelled by its kind")
	}
	if !strings.Contains(body, `/sessions/s1/conversation?hl=flaky#ev-ev2`) {
		t.Error("the hit does not land on the event's anchor in the reader")
	}
	body = get(t, s, "/sessions?q=flaky&harness=1").Body.String()
	if n := len(f.searches); n == 0 || !f.searches[n-1].IncludeHarness {
		t.Fatal("harness=1 did not reach the store")
	}
	if !strings.Contains(body, `class="pick on" href="/sessions?q=flaky"`) || !strings.Contains(body, `name="harness" value="1"`) {
		t.Error("the on-state toggle does not offer to switch off, or the form drops it")
	}
}
