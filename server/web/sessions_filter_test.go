package web

// These tests exist because the session list is the one page where a broken
// filter is invisible. The production corpus is a single session, so a filter
// that returns one row for a matching value and none for a non-matching one is
// byte-identical to a filter that is ignored and to one that is dropped on the
// way to storage. The only way to tell the three apart is a corpus wide enough
// that "ignored" and "applied" produce different pages, which is what fltData
// below is for: it applies the same predicates the SQL in store.ListSessions
// applies, so a filter the handler forgets to pass shows up as extra rows on
// the page rather than as a query object that happens to carry the right field.
//
// The fixtures are deliberately not the shared fakeData: that one filters on
// repo alone, which makes every other filter pass by doing nothing.
//
// Scope is the metadata filter and nothing else. The message search the list
// also runs for a text query is left returning nothing here, so a row on the
// page is a row the filter selected rather than one a transcript match dragged
// in; transcript matching has its own tests next door.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// fltData is a Data whose ListSessions applies every filter the port declares.
// It embeds the shared fake for everything else, so a change to the port breaks
// one type rather than two.
type fltData struct {
	*fakeData
	all []Session
}

// fltDay builds a start time in the location parseDay reads dates in, so the
// boundary cases mean the same thing to the fixture and to the handler however
// the test host is configured.
func fltDay(day, hour int) time.Time {
	return time.Date(2026, 8, day, hour, 0, 0, 0, time.Local)
}

func newFilterData() *fltData {
	return &fltData{
		fakeData: newFake(),
		all: []Session{
			{ID: "s-a", Email: "ann@example.com", Repo: "backend", Source: "claude_code", StartedAt: fltDay(1, 9), FirstPrompt: "deploy the credential service"},
			{ID: "s-b", Email: "ann@example.com", Repo: "frontend", Source: "codex", StartedAt: fltDay(2, 9), FirstPrompt: "fix the flaky test"},
			{ID: "s-c", Email: "bob@example.com", Repo: "backend", Source: "codex", StartedAt: fltDay(3, 9), FirstPrompt: "write the migration"},
			{ID: "s-d", Email: "bob@example.com", Repo: "infra", Source: "claude_code", StartedAt: fltDay(4, 9), FirstPrompt: "deploy terraform"},
			{ID: "s-e", Email: "cara@example.com", Repo: "backend", Source: "claude_code", StartedAt: fltDay(5, 9), FirstPrompt: "review the PR"},
		},
	}
}

// fltMatches mirrors the WHERE clause of store.ListSessions: exact match on
// email, source and repo, a half-open window on started_at, and a substring
// over the four text columns the free-text box covers.
func fltMatches(s Session, q SessionQuery) bool {
	if q.Email != "" && s.Email != q.Email {
		return false
	}
	if q.Source != "" && s.Source != q.Source {
		return false
	}
	if q.Repo != "" && s.Repo != q.Repo {
		return false
	}
	if len(q.Types) > 0 {
		// A fixture with no explicit type is a user session, as in the store,
		// where automation must be proven.
		ty := s.Type
		if ty == "" {
			ty = "user"
		}
		if !containsType(q.Types, ty) {
			return false
		}
	}
	if !q.From.IsZero() && s.StartedAt.Before(q.From) {
		return false
	}
	if !q.To.IsZero() && !s.StartedAt.Before(q.To) {
		return false
	}
	if q.Q != "" {
		hay := strings.ToLower(s.FirstPrompt + "\x00" + s.Repo + "\x00" + s.Cwd + "\x00" + s.Branch)
		if !strings.Contains(hay, strings.ToLower(q.Q)) {
			return false
		}
	}
	return true
}

func (f *fltData) ListSessions(_ context.Context, v Viewer, q SessionQuery) (SessionPage, error) {
	f.lastQuery = q
	var out []Session
	for _, s := range f.all {
		if s.Email != v.Email && !v.Admin {
			continue
		}
		if fltMatches(s, q) {
			out = append(out, s)
		}
	}
	// Newest first and keyset-paged on the last row of the page, because that is
	// the order and the cursor shape the pager link has to survive; a fake that
	// returned everything in one page could not tell a next-page link that keeps
	// the filters from one that drops them.
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	if q.Cursor != "" {
		for i, s := range out {
			if s.ID == q.Cursor {
				out = out[i+1:]
				break
			}
		}
	}
	var page SessionPage
	if q.Limit > 0 && len(out) > q.Limit {
		page.NextCursor = out[q.Limit-1].ID
		out = out[:q.Limit]
	}
	page.Sessions = out
	return page, nil
}

// fltServer builds a dashboard over fltData. It does not reuse newServer, which
// is typed to the shared fake.
func fltServer(t *testing.T, f *fltData, v Viewer) *Server {
	t.Helper()
	s, err := New(Options{
		Data:    f,
		Viewer:  func(*http.Request) (Viewer, bool) { return v, v.Email != "" },
		CSRFKey: []byte("test-key-for-signing-form-tokens"),
		Now:     func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// fltLinkRE finds which sessions the rendered page actually offers, read back
// out of the hrefs rather than out of the view struct. A filter that narrows the
// query and a page that lists what the query returned are two claims, and only
// the second is what the operator sees.
var fltLinkRE = regexp.MustCompile(`href="/sessions/(s-[a-z])`)

func fltLinked(body string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range fltLinkRE.FindAllStringSubmatch(body, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	sort.Strings(out)
	return out
}

func fltGet(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// TestSessionListEveryFilterNarrowsToExactlyItsMatches is the property the
// single-row production corpus cannot demonstrate: each control on the filter
// bar changes which sessions the page lists, and none of them is accepted and
// then ignored.
func TestSessionListEveryFilterNarrowsToExactlyItsMatches(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  []string
	}{
		{"no filter lists everything the viewer may read", "", []string{"s-a", "s-b", "s-c", "s-d", "s-e"}},
		{"person narrows to one owner", "?email=ann%40example.com", []string{"s-a", "s-b"}},
		{"repo narrows to one repository", "?repo=backend", []string{"s-a", "s-c", "s-e"}},
		{"tool narrows to one harness", "?source=codex", []string{"s-b", "s-c"}},
		{"from is an inclusive lower bound on the start date", "?from=2026-08-03", []string{"s-c", "s-d", "s-e"}},
		{"to covers the whole of the day named", "?to=2026-08-02", []string{"s-a", "s-b"}},
		{"from and to together bound both ends", "?from=2026-08-02&to=2026-08-03", []string{"s-b", "s-c"}},
		{"contains matches the first prompt", "?q=deploy", []string{"s-a", "s-d"}},
		{"contains also matches the repository", "?q=frontend", []string{"s-b"}},
		{"filters compose rather than replacing each other", "?repo=backend&source=claude_code", []string{"s-a", "s-e"}},
		{"a value nothing matches empties the list", "?repo=nosuchrepo", nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFilterData()
			s := fltServer(t, f, admin)
			rec := fltGet(t, s, "/sessions"+tc.query)
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d for %q\n%s", rec.Code, tc.query, rec.Body.String())
			}
			got := fltLinked(rec.Body.String())
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("%q listed %v, want %v", tc.query, got, tc.want)
			}
		})
	}
}

// TestSessionListFormRedisplaysEveryFilterItWasGiven defends the other half of a
// working filter bar. A page that narrows correctly and then draws itself with
// empty boxes reads as a filter that did nothing, and the next thing the
// operator does is type it again.
func TestSessionListFormRedisplaysEveryFilterItWasGiven(t *testing.T) {
	f := newFilterData()
	s := fltServer(t, f, admin)
	body := fltGet(t, s,
		"/sessions?q=deploy&email=ann%40example.com&repo=backend&source=codex&from=2026-08-01&to=2026-08-05").
		Body.String()

	for _, want := range []string{
		`name="q" value="deploy"`,
		`name="email" value="ann@example.com"`,
		`name="repo" value="backend"`,
		`name="from" value="2026-08-01"`,
		`name="to" value="2026-08-05"`,
		// The tool menu's button shows the applied value, and its option list
		// marks it current — the menu equivalent of a selected <option>.
		`<summary>codex</summary>`,
		`class="on" href="/sessions?email=ann%40example.com&amp;from=2026-08-01&amp;q=deploy&amp;repo=backend&amp;source=codex&amp;to=2026-08-05">codex</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the filter bar came back without %s", want)
		}
	}
}

// TestSessionListOffersEveryHarnessTheCorpusCanHold guards the one control that
// cannot redisplay a value it was never able to offer. The dropdown is a fixed
// list, so a harness that exists in event.Source and not here is a filter that
// silently cannot be selected, which on a corpus of that harness reads as a
// session list that lost everything.
func TestSessionListOffersEveryHarnessTheCorpusCanHold(t *testing.T) {
	f := newFilterData()
	s := fltServer(t, f, admin)
	body := fltGet(t, s, "/sessions").Body.String()

	seen := map[string]bool{}
	for _, sess := range f.all {
		seen[sess.Source] = true
	}
	for source := range seen {
		// An option is now a link that applies itself; the assertion is that a
		// link setting this source exists at all.
		if !strings.Contains(body, `?source=`+source+`"`) {
			t.Errorf("sessions recorded with source %q cannot be filtered for: no option offers it", source)
		}
	}
}

// TestSessionListPagerKeepsTheFilters covers the case where the filter survives
// one page and not the next: a second page fetched without them silently widens
// the answer under a heading that still says it is filtered.
func TestSessionListPagerKeepsTheFilters(t *testing.T) {
	f := newFilterData()
	s := fltServer(t, f, admin)
	s.pageN = 1

	body := fltGet(t, s, "/sessions?repo=backend&source=claude_code").Body.String()
	i := strings.Index(body, `class="btn" href="/sessions?`)
	if i < 0 {
		t.Fatal("a filtered list with more rows than the page offers no next page")
	}
	next := body[i+len(`class="btn" href="`):]
	next = next[:strings.Index(next, `"`)]
	for _, want := range []string{"repo=backend", "source=claude_code", "cursor="} {
		if !strings.Contains(next, want) {
			t.Errorf("the next-page link %q drops %s", next, want)
		}
	}
}
