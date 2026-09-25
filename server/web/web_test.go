package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

var (
	owner  = Viewer{Email: "owner@example.com", Name: "Owner"}
	other  = Viewer{Email: "nosy@example.com", Name: "Nosy"}
	admin  = Viewer{Email: "boss@example.com", Name: "Boss", Admin: true}
	nobody = Viewer{}
)

func get(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestEveryPageRenders(t *testing.T) {
	f := newFake()
	f.seedSession("s1", admin.Email)
	f.seedFleet()
	f.people = []Principal{
		{Email: admin.Email, DisplayName: "Boss", Role: RoleAdmin, AddedAt: fixedNow.Add(-100 * time.Hour), Sessions: 4},
		{Email: owner.Email, Role: RoleMember, AddedAt: fixedNow.Add(-50 * time.Hour)},
	}
	f.hits = []SearchHit{{
		Session: f.sessions["s1"].Session, EventID: "ev2", Seq: 2,
		Role: "user", OccurredAt: fixedNow, Text: "fix the flaky test in main.go",
	}}
	f.access = []AccessEntry{{Viewer: admin.Email, SessionID: "s1", Owner: owner.Email, Via: "admin", At: fixedNow}}
	s := newServer(t, f, admin)

	for _, path := range []string{
		"/sessions",
		"/sessions?repo=acme/api&from=2026-08-01",
		"/sessions/s1",
		"/sessions/s1?after=3",
		"/sessions/s1?agent=agent-1",
		"/sessions/s1/events/ev5",
		// Search has no page of its own: the query is a filter on the list, and
		// the list is what has to render with one applied.
		"/sessions?q=flaky",
		"/admin/principals",
		"/admin/fleet",
		"/admin/access",
	} {
		rec := get(t, s, path)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status %d, want 200\n%s", path, rec.Code, rec.Body.String())
			continue
		}
		body := rec.Body.String()
		if !strings.Contains(body, "</html>") {
			t.Errorf("%s: response is not a complete document", path)
		}
	}
}

// TestDenialIsIndistinguishableFromAbsence is the core authorization property.
// A viewer who may not read a session must not be able to tell whether it
// exists, so the two responses have to be byte-identical.
func TestDenialIsIndistinguishableFromAbsence(t *testing.T) {
	f := newFake()
	f.seedSession("real", owner.Email)
	s := newServer(t, f, other)

	denied := get(t, s, "/sessions/real")
	missing := get(t, s, "/sessions/does-not-exist")

	if denied.Code != http.StatusNotFound {
		t.Fatalf("denied session: status %d, want 404", denied.Code)
	}
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing session: status %d, want 404", missing.Code)
	}
	if denied.Body.String() != missing.Body.String() {
		t.Fatal("a denied read is distinguishable from a missing one")
	}
	for _, leak := range []string{owner.Email, "acme/api", "flaky"} {
		if strings.Contains(denied.Body.String(), leak) {
			t.Errorf("404 body leaks %q", leak)
		}
	}
}

// TestExplicitDenialAlsoRenders404 covers a store that reports refusal
// explicitly rather than folding it into ErrNotFound.
func TestExplicitDenialAlsoRenders404(t *testing.T) {
	f := newFake()
	f.seedSession("real", owner.Email)
	f.readable = func(Viewer, string) error { return ErrDenied }
	s := newServer(t, f, other)

	rec := get(t, s, "/sessions/real")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404 for an explicit denial", rec.Code)
	}
}

func TestAdminPagesAreInvisibleToMembers(t *testing.T) {
	f := newFake()
	s := newServer(t, f, owner)
	for _, path := range []string{"/admin/principals", "/admin/fleet", "/admin/access"} {
		if rec := get(t, s, path); rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404 for a member", path, rec.Code)
		}
	}
	if body := get(t, s, "/sessions").Body.String(); strings.Contains(body, "/admin/principals") {
		t.Error("the member's navigation advertises the admin pages")
	}
}

func TestUnauthenticatedGetRedirectsToSignIn(t *testing.T) {
	s := newServer(t, newFake(), nobody)
	rec := get(t, s, "/sessions?repo=x")
	if rec.Code != http.StatusFound {
		t.Fatalf("status %d, want 302", rec.Code)
	}
	loc := rec.Header().Get("Location")
	// The default this asserts is only reached by a caller assembling the
	// dashboard by hand; the composition root always supplies the path. It is
	// pinned anyway because the previous default, /auth/google/start, outlived
	// the OAuth flow it belonged to and would have sent every unauthenticated
	// visitor to a 404 that looked like a deliberate destination.
	if !strings.HasPrefix(loc, "/auth/signin?next=") {
		t.Fatalf("Location %q does not start the sign-in flow", loc)
	}
	if !strings.Contains(loc, url.QueryEscape("/sessions?repo=x")) {
		t.Errorf("Location %q loses the page the viewer asked for", loc)
	}
}

// TestTranscriptContentIsEscaped is the property the whole no-JavaScript
// posture exists to protect. Transcripts carry markup from pages the agent
// fetched, and none of it may reach the browser as markup.
func TestTranscriptContentIsEscaped(t *testing.T) {
	f := newFake()
	f.seedSession("s1", owner.Email)
	s := newServer(t, f, owner)

	body := get(t, s, "/sessions/s1").Body.String()
	for _, raw := range []string{
		`<script>alert("xss")</script>`,
		`<img src=x onerror="alert(1)">`,
		`println("<b>hi</b>")`,
	} {
		if strings.Contains(body, raw) {
			t.Errorf("unescaped transcript content in the response: %q", raw)
		}
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Error("the prompt's markup was not rendered as escaped text at all")
	}
	if !strings.Contains(body, "&lt;img src=x onerror=") {
		t.Error("tool output markup was not rendered as escaped text")
	}
	if !strings.Contains(body, "&lt;b&gt;hi&lt;/b&gt;") {
		t.Error("diff content markup was not rendered as escaped text")
	}
}

// TestHighlightDoesNotBypassEscaping checks the one place where building HTML
// in Go would be tempting: a search term that matches inside markup must still
// come out as text wrapped in a mark element.
func TestHighlightDoesNotBypassEscaping(t *testing.T) {
	f := newFake()
	f.seedSession("s1", owner.Email)
	s := newServer(t, f, owner)

	body := get(t, s, "/sessions/s1?hl=script").Body.String()
	if strings.Contains(body, "<script>") {
		t.Fatal("highlighting emitted live markup")
	}
	if !strings.Contains(body, "<mark>script</mark>") {
		t.Fatal("the match was not marked")
	}
	if !strings.Contains(body, "&lt;<mark>script</mark>&gt;") {
		t.Error("the text around the match was not escaped")
	}
}

// TestTheOneScriptStaysSmallAndInert bounds what the 2026-08-13 script budget
// bought.
//
// The filter pages gave up script-src 'none' to load exactly one file. That
// trade holds only while the file stays a thing a person can read in a sitting
// and cannot be talked into executing text: nothing here should ever build DOM
// from a string, evaluate one, or fetch anything. A dependency arriving in this
// directory is the shape of the change this refuses.
func TestTheOneScriptStaysSmallAndInert(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("static", "combo.js"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	for _, bad := range []string{
		"eval(", "innerHTML", "outerHTML", "insertAdjacentHTML",
		"document.write", "new Function", "fetch(", "XMLHttpRequest", "import(",
	} {
		if strings.Contains(src, bad) {
			t.Errorf("combo.js uses %s, which is more than a dropdown needs", bad)
		}
	}
	// The budget counts code, not comments. Counting every line made the test
	// fire when the ARIA wiring arrived with the paragraph explaining why the
	// widget owes screen readers something the datalist gave them for free,
	// which is a test punishing the thing it should want. Generous, and still
	// an order of magnitude under any framework: a rewrite into something
	// larger has to argue for itself here.
	var code int
	for _, line := range strings.Split(src, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "//") {
			code++
		}
	}
	if code > 200 {
		t.Errorf("combo.js is %d lines of code; the budget was one small file", code)
	}
	// The static handler types .js correctly, and every response carries
	// nosniff, so a wrong type is a script the browser silently refuses.
	f := newFake()
	rec := get(t, newServer(t, f, owner), "/static/combo.js")
	if rec.Code != http.StatusOK {
		t.Fatalf("combo.js is not served: %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Errorf("combo.js served as %q, which nosniff makes unexecutable", ct)
	}
}

// TestNoTemplateHTMLInPackage guards the invariant by construction rather than
// by review: any future use of template.HTML, template.JS or template.URL in
// this package would let unescaped content through, and the transcript path is
// where that content comes from.
func TestNoTemplateHTMLInPackage(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	banned := []string{"template.HTML(", "template.JS(", "template.URL(", "template.HTMLAttr("}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range banned {
			if strings.Contains(string(b), bad) {
				t.Errorf("%s uses %s, which defeats contextual escaping", name, bad)
			}
		}
	}
}

func TestSecurityHeaders(t *testing.T) {
	f := newFake()
	s := newServer(t, f, owner)
	h := get(t, s, "/sessions").Header()

	csp := h.Get("Content-Security-Policy")
	// The session list is a filter page, so it may load same-origin script and
	// nothing else. Inline script stays refused here exactly as it is on a
	// transcript, which is the part that matters: this page prints search
	// snippets drawn out of transcripts, and 'self' without 'unsafe-inline'
	// still turns an injected <script>…</script> into inert text.
	if !strings.Contains(csp, "script-src 'self'") {
		t.Errorf("CSP %q does not allow the combo script", csp)
	}
	for _, banned := range []string{"'unsafe-inline'", "'unsafe-eval'", "http:", "https:", "*"} {
		if strings.Contains(csp, banned) {
			t.Errorf("CSP %q widens script beyond this origin with %s", csp, banned)
		}
	}
	if !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("CSP %q allows framing", csp)
	}
	if got := h.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", got)
	}
	if got := h.Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Errorf("Cache-Control = %q; transcripts must not be cached", got)
	}
}

func TestStaticIsServedAndFingerprinted(t *testing.T) {
	f := newFake()
	s := newServer(t, f, owner)

	page := get(t, s, "/sessions").Body.String()
	i := strings.Index(page, "/static/app.css?v=")
	if i < 0 {
		t.Fatal("the page does not reference a fingerprinted stylesheet")
	}
	href := page[i : i+strings.Index(page[i:], `"`)]

	rec := get(t, s, href)
	if rec.Code != http.StatusOK {
		t.Fatalf("stylesheet status %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Errorf("Content-Type = %q", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("fingerprinted asset Cache-Control = %q, want immutable", cc)
	}
	if cc := get(t, s, "/static/app.css").Header().Get("Cache-Control"); strings.Contains(cc, "immutable") {
		t.Error("an unfingerprinted asset must not be cached immutably")
	}
	if rec := get(t, s, "/static/../web.go"); rec.Code == http.StatusOK {
		t.Error("path traversal was served")
	}
}

func TestListShowsCostAndDurationWithoutClickingThrough(t *testing.T) {
	f := newFake()
	f.seedSession("s1", owner.Email)
	s := newServer(t, f, owner)

	body := get(t, s, "/sessions").Body.String()
	if !strings.Contains(body, "$0.42") {
		t.Error("the list does not show cost")
	}
	if !strings.Contains(body, "22m 00s") {
		t.Error("the list does not show duration")
	}
	if !strings.Contains(body, "fix the flaky test") {
		t.Error("the list does not show the first prompt")
	}
}

func TestListFiltersReachStorage(t *testing.T) {
	f := newFake()
	s := newServer(t, f, admin)
	get(t, s, "/sessions?q=deploy&email=a%40example.com&repo=r&source=codex&from=2026-08-01&to=2026-08-02")

	q := f.lastQuery
	if q.Q != "deploy" || q.Email != "a@example.com" || q.Repo != "r" || q.Source != "codex" {
		t.Fatalf("filters lost on the way to storage: %+v", q)
	}
	if q.From.IsZero() || q.To.IsZero() {
		t.Fatalf("date range lost: %+v", q)
	}
	if !q.To.After(q.From) {
		t.Errorf("To (%s) is not after From (%s)", q.To, q.From)
	}
}

// TestTranscriptPaginates checks that a large session is read in windows and
// that the window links carry the cursor, since the alternative is a page that
// tries to render everything.
// TestTranscriptPaginates: the page is twenty-five whole turns, the cursor is
// the last turn's index, and a deeper page links back to the start.
func TestTranscriptPaginates(t *testing.T) {
	f := newFake()
	f.seedSession("s1", owner.Email)
	f.turns["s1"] = manyTurns(f.events["s1"][1], 40)
	s := newServer(t, f, owner)

	body := get(t, s, "/sessions/s1").Body.String()
	if f.lastTurns.Limit != turnsPerPage {
		t.Fatalf("turn limit = %d, want %d", f.lastTurns.Limit, turnsPerPage)
	}
	if !strings.Contains(body, "after=") {
		t.Fatal("no cursor link to the next page")
	}
	if strings.Contains(body, `id="t25"`) {
		t.Error("a turn beyond the page was rendered")
	}

	// The cursor in the page round-trips: following it must hand the fake a
	// decodable position, and the deeper page links back to the start.
	m := regexp.MustCompile(`after=([0-9]+)`).FindStringSubmatch(body)
	if m == nil {
		t.Fatal("cursor is not a turn index")
	}
	second := get(t, s, "/sessions/s1?after="+m[1]).Body.String()
	if f.lastTurns.After == nil || *f.lastTurns.After != 24 {
		t.Fatalf("the followed cursor did not reach the data layer: %v", f.lastTurns.After)
	}
	if !strings.Contains(second, `href="/sessions/s1"`) {
		t.Error("no link back to the start of the session")
	}
	if !strings.Contains(second, `id="t25"`) || strings.Contains(second, `id="t24"`) {
		t.Error("the second page did not start after the cursor")
	}
}

func TestSharedLinkResolvesToTheSession(t *testing.T) {
	f := newFake()
	f.seedSession("s1", owner.Email)
	f.shares["tok"] = "s1"
	s := newServer(t, f, other)

	rec := get(t, s, "/shared/tok")
	if rec.Code != http.StatusFound {
		t.Fatalf("status %d, want a redirect to the session", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/sessions/s1" {
		t.Fatalf("Location = %q", loc)
	}
	if rec := get(t, s, "/shared/bogus"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown token: status %d, want 404", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Admin writes
// ---------------------------------------------------------------------------

func postPrincipal(t *testing.T, s *Server, email string, form url.Values, origin string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/admin/principals/"+url.PathEscape(email),
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func adminFake() *fakeData {
	f := newFake()
	f.people = []Principal{
		{Email: admin.Email, Role: RoleAdmin},
		{Email: "second@example.com", Role: RoleAdmin},
		{Email: owner.Email, Role: RoleMember},
	}
	return f
}

func TestPrincipalUpdateSaves(t *testing.T) {
	f := adminFake()
	s := newServer(t, f, admin)
	form := url.Values{"role": {RoleAdmin}, "csrf": {s.csrfToken(admin)}}

	rec := postPrincipal(t, s, owner.Email, form, "")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want 303", rec.Code)
	}
	if len(f.saved) != 1 || f.saved[0].Email != owner.Email || f.saved[0].Role != RoleAdmin {
		t.Fatalf("storage received %+v", f.saved)
	}
}

func TestPrincipalUpdateRequiresCSRFToken(t *testing.T) {
	f := adminFake()
	s := newServer(t, f, admin)

	rec := postPrincipal(t, s, owner.Email, url.Values{"role": {RoleAdmin}}, "")
	if len(f.saved) != 0 {
		t.Fatal("a form with no token changed a role")
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "err=stale") {
		t.Errorf("Location = %q, want the stale-form message", loc)
	}
}

func TestPrincipalUpdateRejectsAnotherViewersToken(t *testing.T) {
	f := adminFake()
	s := newServer(t, f, admin)
	form := url.Values{"role": {RoleAdmin}, "csrf": {s.csrfToken(owner)}}

	postPrincipal(t, s, owner.Email, form, "")
	if len(f.saved) != 0 {
		t.Fatal("a token minted for a different viewer was accepted")
	}
}

func TestPrincipalUpdateRejectsCrossOriginPost(t *testing.T) {
	f := adminFake()
	s := newServer(t, f, admin)
	form := url.Values{"role": {RoleAdmin}, "csrf": {s.csrfToken(admin)}}

	rec := postPrincipal(t, s, owner.Email, form, "https://evil.example")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403", rec.Code)
	}
	if len(f.saved) != 0 {
		t.Fatal("a cross-origin post changed a role")
	}
}

func TestPrincipalUpdateRefusesToRemoveTheLastAdmin(t *testing.T) {
	f := newFake()
	f.people = []Principal{
		{Email: admin.Email, Role: RoleAdmin},
		{Email: owner.Email, Role: RoleMember},
	}
	s := newServer(t, f, admin)
	form := url.Values{"role": {RoleMember}, "csrf": {s.csrfToken(admin)}}

	rec := postPrincipal(t, s, admin.Email, form, "")
	if len(f.saved) != 0 {
		t.Fatal("the last admin was demoted")
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "err=last_admin") {
		t.Errorf("Location = %q, want the last-admin message", loc)
	}
}

func TestPrincipalUpdateCountsDisablingAsRemoval(t *testing.T) {
	f := newFake()
	f.people = []Principal{{Email: admin.Email, Role: RoleAdmin}}
	s := newServer(t, f, admin)
	form := url.Values{"role": {RoleAdmin}, "disabled": {"on"}, "csrf": {s.csrfToken(admin)}}

	postPrincipal(t, s, admin.Email, form, "")
	if len(f.saved) != 0 {
		t.Fatal("disabling the only admin was allowed")
	}
}

func TestMembersCannotPostToTheAdminPage(t *testing.T) {
	f := adminFake()
	s := newServer(t, f, owner)
	form := url.Values{"role": {RoleAdmin}, "csrf": {s.csrfToken(owner)}}

	rec := postPrincipal(t, s, owner.Email, form, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", rec.Code)
	}
	if len(f.saved) != 0 {
		t.Fatal("a member changed a role")
	}
}

func TestCSRFTokenExpires(t *testing.T) {
	f := adminFake()
	s := newServer(t, f, admin)
	tok := s.csrfToken(admin)

	s.now = func() time.Time { return fixedNow.Add(csrfTTL + time.Minute) }
	if s.csrfValid(admin, tok) {
		t.Fatal("an expired token was accepted")
	}
}

// What TestSearchMarksMatches defended — the mark, the link into the window
// containing the hit, and the disclosure of a capped ranking window — now lives
// in sessions_search_test.go, next to the rest of the merged list's behaviour.
