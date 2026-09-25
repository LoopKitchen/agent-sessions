package web

// The session list absorbed the search page. These tests defend what that move
// is allowed to cost: the ranked, highlighted snippet the search page rendered
// well has to survive on the list, a query that matches only inside a
// transcript still has to surface the session it matched in, and the URL people
// already have in their history has to keep working.

import (
	"net/http"
	"strings"
	"testing"
)

// TestSessionListMarksWhereAQueryMatchedInsideATranscript is the property the
// fold-in exists to preserve. A session whose own metadata says nothing about
// the query still has to show the reader the words that matched and where in
// the transcript they are, which is the whole reason the search page was worth
// keeping the store's ts_headline for.
func TestSessionListMarksWhereAQueryMatchedInsideATranscript(t *testing.T) {
	f := newFake()
	f.seedSession("s1", owner.Email)
	f.hits = []SearchHit{{
		Session: Session{ID: "s1", Email: owner.Email}, EventID: "ev5", Seq: 4,
		Role: "tool", OccurredAt: fixedNow, Text: "the flaky test is in main.go",
	}}
	s := newServer(t, f, owner)

	body := get(t, s, "/sessions?q=flaky").Body.String()
	if !strings.Contains(body, "<mark>flaky</mark>") {
		t.Error("the match inside the transcript is not highlighted on the list")
	}
	if !strings.Contains(body, "the <mark>flaky</mark> test is in main.go") {
		t.Error("the snippet around the match was dropped when search moved onto the list")
	}
	// The link lands the reader on the event itself: the scrollable reader
	// with the event's anchor, whose loader keeps paging until the anchor
	// arrives, which is what makes a match in a twelve-thousand-event session
	// reachable now that pages are turns rather than event windows.
	if !strings.Contains(body, "/sessions/s1/conversation?hl=flaky#ev-ev5") {
		t.Error("the match does not link into the transcript window containing it")
	}
}

// TestSessionListSurfacesMatchesInSessionsItIsNotShowing covers the case the
// list alone cannot answer: the query appears in a transcript belonging to a
// session that the metadata filters, the cursor or the page size leave off this
// page. Dropping those is losing full-text search, which is the one thing the
// fold-in was not allowed to do.
func TestSessionListSurfacesMatchesInSessionsItIsNotShowing(t *testing.T) {
	f := newFake()
	f.seedSession("s1", owner.Email)
	f.hits = []SearchHit{{
		Session: Session{ID: "elsewhere", Email: owner.Email}, EventID: "ev9", Seq: 12,
		Role: "assistant", OccurredAt: fixedNow, Text: "the flaky test only fails under -race",
	}}
	s := newServer(t, f, owner)

	body := get(t, s, "/sessions?q=flaky").Body.String()
	if !strings.Contains(body, "Found inside these transcripts") {
		t.Fatal("a match in a session that is not on the page is not offered at all")
	}
	if !strings.Contains(body, "/sessions/elsewhere/conversation?hl=flaky#ev-ev9") {
		t.Error("the match does not link into the transcript it came from")
	}
	if !strings.Contains(body, "<mark>flaky</mark>") {
		t.Error("the snippet is not highlighted")
	}
}

// TestAListWithNoRowsButMatchesDoesNotCallItselfEmpty covers the page state the
// fold-in introduced: the metadata filter matches nothing and the transcripts
// match something. The empty notice and the results cannot both be on the page,
// because one of them is telling the reader the other is not there.
func TestAListWithNoRowsButMatchesDoesNotCallItselfEmpty(t *testing.T) {
	f := newFake()
	f.hits = []SearchHit{{
		Session: Session{ID: "only-in-the-transcript", Email: owner.Email}, Seq: 8,
		Role: "tool", OccurredAt: fixedNow, Text: "connection refused after the flaky retry",
	}}
	s := newServer(t, f, owner)

	body := get(t, s, "/sessions?q=flaky").Body.String()
	if !strings.Contains(body, "/sessions/only-in-the-transcript") {
		t.Fatal("the only match on the page was not offered")
	}
	if strings.Contains(body, "Nothing here yet") {
		t.Error("the page says it is empty while listing matches")
	}
}

// TestSessionListShowsOneMatchPerSession keeps the list a list. A query that
// hits forty times in one transcript must not push the other thirty-nine
// sessions off the page.
func TestSessionListShowsOneMatchPerSession(t *testing.T) {
	f := newFake()
	f.seedSession("s1", owner.Email)
	hit := SearchHit{
		Session: Session{ID: "elsewhere", Email: owner.Email},
		Role:    "tool", OccurredAt: fixedNow, Text: "flaky here",
	}
	second := hit
	second.Seq, second.Text = 40, "flaky there"
	f.hits = []SearchHit{hit, second}
	s := newServer(t, f, owner)

	body := get(t, s, "/sessions?q=flaky").Body.String()
	if n := strings.Count(body, `class="hit"`); n != 1 {
		t.Errorf("%d results for one session, want the strongest one only", n)
	}
	if strings.Contains(body, "flaky there") {
		t.Error("a weaker hit in a session already listed was rendered as a second result")
	}
}

// TestSessionListDisclosesACappedRankingWindow carries over the search page's
// one piece of honesty about its own limits: a query that matched more than the
// store will rank has to say so, or the reader takes a partial answer for the
// whole one.
func TestSessionListDisclosesACappedRankingWindow(t *testing.T) {
	f := newFake()
	f.seedSession("s1", owner.Email)
	f.hits = []SearchHit{{
		Session: Session{ID: "s1", Email: owner.Email}, Seq: 2,
		Role: "user", OccurredAt: fixedNow, Text: "the flaky test",
	}}
	f.capped = true
	s := newServer(t, f, owner)

	if body := get(t, s, "/sessions?q=flaky").Body.String(); !strings.Contains(body, "ranking window") {
		t.Error("a capped result set is not disclosed on the list")
	}
}

// TestADisclosureAboutTheQueryPrecedesTheResultsItQualifies is what the search
// page got right by having nothing above its results. On a merged list the
// warning has a table under it, and a warning a reader reaches only by scrolling
// past fifty rows is one they act on after taking the page at face value. Both
// of these say the answer is not the whole answer, so both have to be readable
// before the answer is.
func TestADisclosureAboutTheQueryPrecedesTheResultsItQualifies(t *testing.T) {
	cases := []struct {
		name   string
		path   string
		phrase string
		setUp  func(*fakeData)
	}{
		{
			name:   "a capped ranking window",
			path:   "/sessions?q=flaky",
			phrase: "ranking window",
			setUp:  func(f *fakeData) { f.capped = true },
		},
		{
			name:   "transcript matching withheld under a repo filter",
			path:   "/sessions?q=flaky&repo=acme/api",
			phrase: "Transcript matching is off",
			setUp:  func(*fakeData) {},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			f.seedSession("s1", owner.Email)
			f.hits = []SearchHit{{
				Session: Session{ID: "s1", Email: owner.Email}, Seq: 2,
				Role: "user", OccurredAt: fixedNow, Text: "the flaky test",
			}}
			tc.setUp(f)
			body := get(t, newServer(t, f, owner), tc.path).Body.String()

			said := strings.Index(body, tc.phrase)
			if said < 0 {
				t.Fatalf("the page never says %q", tc.phrase)
			}
			table := strings.Index(body, `<table class="list"`)
			if table < 0 {
				t.Fatal("no table on a page that listed a session")
			}
			if said > table {
				t.Errorf("%q sits below the results it qualifies", tc.phrase)
			}
		})
	}
}

// TestTranscriptMatchingIsWithheldWhileARepoFilterIsApplied states the one
// filter combination the store cannot answer. Messages carry no repository, so
// a search narrowed by repo either fails or returns other repositories' hits
// under a heading that says otherwise. The page withholds the matches and says
// why, and it must not issue the search at all: the adapter refuses it, and a
// refusal here would take the whole session list down with it.
func TestTranscriptMatchingIsWithheldWhileARepoFilterIsApplied(t *testing.T) {
	f := newFake()
	f.seedSession("s1", owner.Email)
	f.hits = []SearchHit{{
		Session: Session{ID: "elsewhere", Email: owner.Email}, Seq: 3,
		Role: "tool", OccurredAt: fixedNow, Text: "flaky elsewhere",
	}}
	s := newServer(t, f, owner)

	rec := get(t, s, "/sessions?q=flaky&repo=acme/api")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want the list to render anyway", rec.Code)
	}
	if len(f.searches) != 0 {
		t.Errorf("the message search was issued with a repo filter the store cannot honour: %+v", f.searches)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Transcript matching is off while a repo filter is applied") {
		t.Error("the page silently drops transcript matching instead of saying it did")
	}
	if strings.Contains(body, "flaky elsewhere") {
		t.Error("a hit from an unknown repository was listed under a repo filter")
	}
}

// TestListFiltersReachTheMessageSearch checks the filters the store can apply
// to messages actually get there. One that is parsed and then not forwarded
// narrows the table and not the matches, so the same page would answer the same
// question two different ways.
func TestListFiltersReachTheMessageSearch(t *testing.T) {
	f := newFake()
	s := newServer(t, f, admin)
	get(t, s, "/sessions?q=deploy&email=a%40example.com&source=codex&from=2026-08-01&to=2026-08-02")

	if len(f.searches) != 1 {
		t.Fatalf("%d searches issued for one query, want exactly one", len(f.searches))
	}
	q := f.searches[0]
	if q.Q != "deploy" || q.Email != "a@example.com" || q.Source != "codex" {
		t.Fatalf("filters lost on the way to the message search: %+v", q)
	}
	if q.From.IsZero() || q.To.IsZero() {
		t.Fatalf("date range lost on the way to the message search: %+v", q)
	}
	// Repo is never forwarded: the store cannot narrow messages by repository,
	// so the page withholds matches instead of asking for the impossible.
	if q.Repo != "" {
		t.Errorf("repo %q was forwarded to a search that cannot honour it", q.Repo)
	}
}

// TestAnUnfilteredListDoesNotSearchMessages pins the cost of the fold-in. The
// list is the page everyone lands on, and paying for a ranked full-text query
// on every visit to it would be a tax on the common case for nobody's benefit.
func TestAnUnfilteredListDoesNotSearchMessages(t *testing.T) {
	f := newFake()
	f.seedSession("s1", owner.Email)
	s := newServer(t, f, owner)

	get(t, s, "/sessions")
	if len(f.searches) != 0 {
		t.Errorf("the bare session list issued %d message searches", len(f.searches))
	}
}

// TestSearchPathRedirectsToTheEquivalentList keeps every /search link that
// already exists in somebody's history working. A bookmark that 404s is a
// worse answer than one that lands on the page the query now lives on, and the
// redirect is permanent because the page it points at is not coming back.
func TestSearchPathRedirectsToTheEquivalentList(t *testing.T) {
	cases := []struct {
		name string
		from string
		want string
	}{
		{"the bare search page becomes the bare list", "/search", "/sessions"},
		{"a query becomes the list's own query", "/search?q=flaky", "/sessions?q=flaky"},
		{"a phrase survives the encoding", "/search?q=connection+refused", "/sessions?q=connection+refused"},
		{"the other filters travel with it", "/search?q=flaky&email=a%40example.com&from=2026-08-01",
			"/sessions?email=a%40example.com&from=2026-08-01&q=flaky"},
		{"a filter the list keeps and search never honoured is not dropped", "/search?q=flaky&repo=backend",
			"/sessions?q=flaky&repo=backend"},
	}

	f := newFake()
	s := newServer(t, f, owner)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := get(t, s, tc.from)
			if rec.Code != http.StatusPermanentRedirect {
				t.Fatalf("status %d, want 308", rec.Code)
			}
			if loc := rec.Header().Get("Location"); loc != tc.want {
				t.Errorf("Location = %q, want %q", loc, tc.want)
			}
		})
	}
}

// TestSearchPathRedirectsBeforeAskingWhoIsAsking keeps the query intact for
// somebody arriving on an old link with an expired session. Requiring a viewer
// first would send them to sign-in with /search as the return address, and the
// return trip would land on the redirect again; redirecting first means the
// sign-in they get sent to carries the list URL they actually wanted.
func TestSearchPathRedirectsBeforeAskingWhoIsAsking(t *testing.T) {
	s := newServer(t, newFake(), nobody)

	rec := get(t, s, "/search?q=flaky")
	if rec.Code != http.StatusPermanentRedirect {
		t.Fatalf("status %d, want the redirect regardless of who is asking", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/sessions?q=flaky" {
		t.Errorf("Location = %q", loc)
	}
}

// TestNothingOffersTheSearchPageAnyMore is the user-visible half of the change:
// a second destination that lists sessions differently is the thing being
// removed, so no page may still advertise one.
func TestNothingOffersTheSearchPageAnyMore(t *testing.T) {
	f := newFake()
	f.seedSession("s1", admin.Email)
	f.seedFleet()
	s := newServer(t, f, admin)

	for _, path := range []string{"/sessions", "/sessions/s1", "/admin/principals", "/admin/fleet"} {
		body := get(t, s, path).Body.String()
		if strings.Contains(body, `href="/search"`) {
			t.Errorf("%s still links to the search page", path)
		}
		if !strings.Contains(body, `<form class="find" action="/sessions"`) {
			t.Errorf("%s: the header search box does not aim at the session list", path)
		}
	}
}
