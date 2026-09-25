package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The fakes below stand in for the store and the credential layer so that every
// property this package is responsible for can be observed without a database:
// which status code a storage answer becomes, what the cursor does when rows
// arrive mid-scroll, and that a failed audit write takes the read down with it.
// A test that needed Postgres to check the 404-not-403 rule would be a test
// that runs rarely enough for the rule to regress between runs.

var testNow = time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)

type auditRow struct {
	viewer    string
	sessionID string
	owner     string
}

// fakeStore implements Store with the same authorization shape as the real one:
// the permission rule is applied inside every read, denial is indistinguishable
// from absence, and an audited read that cannot write its audit row fails.
type fakeStore struct {
	mu sync.Mutex

	principals map[string]Principal
	sessions   []Session
	events     map[string][]StoredEvent
	// superseded are the event ids the derive runner elected out (hook
	// copies with a transcript twin); the store leaves them off a page
	// unless the range asks for them, and so does this fake.
	superseded map[string]bool
	shares     []Share
	hits       []Hit

	// candidateCap mirrors the store's ranking bound so search paging can be
	// exercised against a saturating candidate count.
	candidateCap int

	// auditErr makes every audit write fail, which must fail the read.
	auditErr error
	audits   []auditRow

	principalErr error
	listErr      error

	lastSessionFilter SessionFilter
	lastSearchFilter  SearchFilter
	lastEventRange    EventRange
	lastShareRequest  ShareRequest
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		principals:   map[string]Principal{},
		events:       map[string][]StoredEvent{},
		candidateCap: 500,
	}
}

func (f *fakeStore) addPrincipal(p Principal) { f.principals[p.Email] = p }

func (f *fakeStore) Principal(_ context.Context, email string) (Principal, bool, error) {
	if f.principalErr != nil {
		return Principal{}, false, f.principalErr
	}
	p, ok := f.principals[email]
	return p, ok, nil
}

func (f *fakeStore) canRead(v Viewer, s Session) bool {
	if s.Email == v.Email || v.IsAdmin() {
		return true
	}
	for _, sh := range f.shares {
		if sh.SessionID != s.SessionID || sh.RevokedAt != nil {
			continue
		}
		if sh.ExpiresAt != nil && !sh.ExpiresAt.After(testNow) {
			continue
		}
		if sh.Grantee == "" || sh.Grantee == v.Email {
			return true
		}
	}
	return false
}

func (f *fakeStore) find(sessionID string) (Session, bool) {
	for _, s := range f.sessions {
		if s.SessionID == sessionID {
			return s, true
		}
	}
	return Session{}, false
}

// audit records the read, or fails it. Reading your own work is not audited,
// which is why the failure injection only bites on somebody else's.
func (f *fakeStore) audit(v Viewer, s Session) error {
	if s.Email == v.Email {
		return nil
	}
	if f.auditErr != nil {
		return f.auditErr
	}
	f.audits = append(f.audits, auditRow{viewer: v.Email, sessionID: s.SessionID, owner: s.Email})
	return nil
}

func (f *fakeStore) ListSessions(_ context.Context, v Viewer, q SessionFilter) (SessionPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastSessionFilter = q
	if f.listErr != nil {
		return SessionPage{}, f.listErr
	}
	after, err := decodeFakeCursor(q.Cursor)
	if err != nil {
		return SessionPage{}, err
	}

	var out []Session
	for _, s := range f.sessions {
		if !f.canRead(v, s) {
			continue
		}
		if q.Email != "" && s.Email != q.Email {
			continue
		}
		if q.Source != "" && s.Source != q.Source {
			continue
		}
		if q.Repo != "" && s.Repo != q.Repo {
			continue
		}
		if !q.From.IsZero() && s.StartedAt.Before(q.From) {
			continue
		}
		if !q.To.IsZero() && !s.StartedAt.Before(q.To) {
			continue
		}
		out = append(out, s)
	}
	// Newest first, ties broken by id, which is the ordering the cursor names a
	// position in.
	sort.Slice(out, func(i, j int) bool {
		if !out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].StartedAt.After(out[j].StartedAt)
		}
		return out[i].SessionID > out[j].SessionID
	})
	if after != nil {
		var rest []Session
		for _, s := range out {
			if s.StartedAt.Before(after.startedAt) ||
				(s.StartedAt.Equal(after.startedAt) && s.SessionID < after.sessionID) {
				rest = append(rest, s)
			}
		}
		out = rest
	}

	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	page := SessionPage{Sessions: out}
	if len(out) > limit {
		page.Sessions = out[:limit]
		last := page.Sessions[limit-1]
		page.NextCursor = encodeFakeCursor(last.StartedAt, last.SessionID)
	}
	return page, nil
}

func (f *fakeStore) GetSession(_ context.Context, v Viewer, sessionID string) (Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.find(sessionID)
	if !ok || !f.canRead(v, s) {
		return Session{}, ErrNotFound
	}
	if err := f.audit(v, s); err != nil {
		return Session{}, err
	}
	return s, nil
}

func (f *fakeStore) GetEvents(_ context.Context, v Viewer, sessionID string, r EventRange) (EventPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastEventRange = r
	s, ok := f.find(sessionID)
	if !ok || !f.canRead(v, s) {
		return EventPage{}, ErrNotFound
	}
	if err := f.audit(v, s); err != nil {
		return EventPage{}, err
	}
	var out []StoredEvent
	for _, e := range f.events[sessionID] {
		if r.AfterSeq != nil && e.Seq <= *r.AfterSeq {
			continue
		}
		if f.superseded[e.ID] && !r.IncludeSuperseded {
			continue
		}
		out = append(out, e)
	}
	limit := r.Limit
	if limit <= 0 {
		limit = 50
	}
	page := EventPage{Events: out}
	if len(out) > limit {
		page.Events = out[:limit]
		next := page.Events[limit-1].Seq
		page.HasMore = true
		page.NextAfter = &next
	}
	return page, nil
}

func (f *fakeStore) SearchMessages(_ context.Context, v Viewer, q SearchFilter) (SearchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastSearchFilter = q

	var matched []Hit
	for _, h := range f.hits {
		s, ok := f.find(h.SessionID)
		if !ok || !f.canRead(v, s) {
			continue
		}
		if q.Email != "" && h.Email != q.Email {
			continue
		}
		if !strings.Contains(strings.ToLower(h.Snippet), strings.ToLower(q.Query)) {
			continue
		}
		matched = append(matched, h)
	}
	res := SearchResult{Candidates: len(matched)}
	if res.Candidates > f.candidateCap {
		res.Candidates = f.candidateCap
		res.Capped = true
		matched = matched[:f.candidateCap]
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	if q.Offset < len(matched) {
		end := min(q.Offset+limit, len(matched))
		res.Hits = matched[q.Offset:end]
	}

	seen := map[string]bool{}
	for _, h := range res.Hits {
		if h.Email == v.Email || seen[h.SessionID] {
			continue
		}
		seen[h.SessionID] = true
		s, _ := f.find(h.SessionID)
		if err := f.audit(v, s); err != nil {
			return SearchResult{}, err
		}
	}
	return res, nil
}

func (f *fakeStore) ListShares(_ context.Context, v Viewer, sessionID string) ([]Share, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.find(sessionID)
	if !ok || (s.Email != v.Email && !v.IsAdmin()) {
		return nil, nil
	}
	var out []Share
	for _, sh := range f.shares {
		if sh.SessionID == sessionID {
			out = append(out, sh)
		}
	}
	return out, nil
}

func (f *fakeStore) CreateShare(_ context.Context, v Viewer, req ShareRequest) (Share, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastShareRequest = req
	s, ok := f.find(req.SessionID)
	if !ok || (s.Email != v.Email && !v.IsAdmin()) {
		return Share{}, ErrNotFound
	}
	if err := f.audit(v, s); err != nil {
		return Share{}, err
	}
	sh := Share{
		ID:        fmt.Sprintf("share-%d", len(f.shares)+1),
		SessionID: req.SessionID,
		CreatedBy: v.Email,
		Grantee:   req.Grantee,
		Token:     fmt.Sprintf("tok-%d", len(f.shares)+1),
		CreatedAt: testNow,
	}
	if !req.ExpiresAt.IsZero() {
		at := req.ExpiresAt
		sh.ExpiresAt = &at
	}
	f.shares = append(f.shares, sh)
	return sh, nil
}

func (f *fakeStore) RevokeShare(_ context.Context, v Viewer, shareID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.shares {
		sh := &f.shares[i]
		if sh.ID != shareID || sh.RevokedAt != nil {
			continue
		}
		s, _ := f.find(sh.SessionID)
		if sh.CreatedBy != v.Email && s.Email != v.Email && !v.IsAdmin() {
			return ErrNotFound
		}
		at := testNow
		sh.RevokedAt = &at
		return nil
	}
	return ErrNotFound
}

func (f *fakeStore) ResolveShare(_ context.Context, v Viewer, token string) (Session, Share, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, sh := range f.shares {
		if sh.Token != token || sh.RevokedAt != nil {
			continue
		}
		if sh.ExpiresAt != nil && !sh.ExpiresAt.After(testNow) {
			continue
		}
		if sh.Grantee != "" && sh.Grantee != v.Email {
			continue
		}
		s, ok := f.find(sh.SessionID)
		if !ok || !f.canRead(v, s) {
			return Session{}, Share{}, ErrNotFound
		}
		if err := f.audit(v, s); err != nil {
			return Session{}, Share{}, err
		}
		return s, sh, nil
	}
	return Session{}, Share{}, ErrNotFound
}

// The fake's own cursor is a keyset position, like the real one, so that the
// stability property being tested is a property of the pair rather than of the
// handler pretending on its own.
type fakePosition struct {
	startedAt time.Time
	sessionID string
}

func encodeFakeCursor(t time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(t.UnixNano(), 10) + "|" + id))
}

func decodeFakeCursor(s string) (*fakePosition, error) {
	if s == "" {
		return nil, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, ErrInvalidCursor
	}
	parts := strings.SplitN(string(b), "|", 2)
	if len(parts) != 2 {
		return nil, ErrInvalidCursor
	}
	n, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return nil, ErrInvalidCursor
	}
	return &fakePosition{startedAt: time.Unix(0, n).UTC(), sessionID: parts[1]}, nil
}

// fakeAuth returns whatever identity the test configured.
type fakeAuth struct {
	email string
	err   error
}

func (a *fakeAuth) Authenticate(*http.Request) (Identity, error) {
	if a.err != nil {
		return Identity{}, a.err
	}
	return Identity{Email: a.email}, nil
}

func newHandler(t *testing.T, store Store, auth Authenticator) *Handler {
	t.Helper()
	h, err := New(Options{
		Store: store,
		Auth:  auth,
		Now:   func() time.Time { return testNow },
		// Discard so a test that exercises the 500 path does not print a stack
		// of expected errors into the run.
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h
}

func do(t *testing.T, h *Handler, method, target string, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func decodeBody[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode body %q: %v", w.Body.String(), err)
	}
	return v
}

// seeded builds a store with two people and one session each.
func seeded() *fakeStore {
	f := newFakeStore()
	f.addPrincipal(Principal{Email: "member@example.com", Role: RoleMember})
	f.addPrincipal(Principal{Email: "other@example.com", Role: RoleMember})
	f.addPrincipal(Principal{Email: "admin@example.com", Role: RoleAdmin})
	f.sessions = []Session{
		{SessionID: "own-1", Email: "member@example.com", Source: "claude_code",
			StartedAt: testNow.Add(-2 * time.Hour)},
		{SessionID: "other-1", Email: "other@example.com", Source: "claude_code",
			StartedAt: testNow.Add(-time.Hour)},
	}
	return f
}

func TestUnauthenticatedIsUnauthorized(t *testing.T) {
	f := seeded()
	h := newHandler(t, f, &fakeAuth{err: ErrNoIdentity})

	w := do(t, h, http.MethodGet, "/v1/sessions", "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if got := decodeBody[errorBody](t, w).Error.Code; got != "unauthenticated" {
		t.Fatalf("code = %q, want unauthenticated", got)
	}
}

func TestAuthenticatorFailureIsInternal(t *testing.T) {
	f := seeded()
	h := newHandler(t, f, &fakeAuth{err: errors.New("cookie store unreachable")})

	w := do(t, h, http.MethodGet, "/v1/sessions", "")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	if strings.Contains(w.Body.String(), "unreachable") {
		t.Fatalf("internal detail leaked to the caller: %s", w.Body.String())
	}
}

func TestRosterDecidesAccess(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*fakeStore)
		email string
		want  int
		code  string
	}{
		{
			name:  "not on the roster",
			setup: func(*fakeStore) {},
			email: "stranger@example.com",
			want:  http.StatusForbidden,
			code:  "not_enrolled",
		},
		{
			name: "disabled",
			setup: func(f *fakeStore) {
				f.addPrincipal(Principal{Email: "gone@example.com", Role: RoleMember, Disabled: true})
			},
			email: "gone@example.com",
			want:  http.StatusForbidden,
			code:  "disabled",
		},
		{
			name: "role the roster cannot express",
			setup: func(f *fakeStore) {
				f.addPrincipal(Principal{Email: "odd@example.com", Role: Role("superuser")})
			},
			email: "odd@example.com",
			want:  http.StatusInternalServerError,
			code:  "internal",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := seeded()
			tc.setup(f)
			h := newHandler(t, f, &fakeAuth{email: tc.email})

			w := do(t, h, http.MethodGet, "/v1/sessions", "")
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d (%s)", w.Code, tc.want, w.Body.String())
			}
			if got := decodeBody[errorBody](t, w).Error.Code; got != tc.code {
				t.Fatalf("code = %q, want %q", got, tc.code)
			}
		})
	}
}

func TestRoleIsReadPerRequestNotFromTheCredential(t *testing.T) {
	f := seeded()
	auth := &fakeAuth{email: "admin@example.com"}
	h := newHandler(t, f, auth)

	if w := do(t, h, http.MethodGet, "/v1/sessions/other-1", ""); w.Code != http.StatusOK {
		t.Fatalf("admin read status = %d, want 200", w.Code)
	}
	// The same credential, after the roster changed underneath it, must lose the
	// access it had a moment ago rather than keep it until the cookie expires.
	f.addPrincipal(Principal{Email: "admin@example.com", Role: RoleMember})
	if w := do(t, h, http.MethodGet, "/v1/sessions/other-1", ""); w.Code != http.StatusNotFound {
		t.Fatalf("demoted read status = %d, want 404", w.Code)
	}
}

func TestUnknownRouteAnswersTheSame404AsAForbiddenSession(t *testing.T) {
	f := seeded()
	h := newHandler(t, f, &fakeAuth{email: "member@example.com"})

	forbidden := do(t, h, http.MethodGet, "/v1/sessions/other-1", "")
	unknown := do(t, h, http.MethodGet, "/v1/nothing-here", "")
	if forbidden.Code != http.StatusNotFound || unknown.Code != http.StatusNotFound {
		t.Fatalf("statuses = %d and %d, want 404 and 404", forbidden.Code, unknown.Code)
	}
	if forbidden.Body.String() != unknown.Body.String() {
		t.Fatalf("bodies differ:\n forbidden %s\n unknown   %s", forbidden.Body, unknown.Body)
	}
}

func TestResponsesAreNeverCached(t *testing.T) {
	f := seeded()
	h := newHandler(t, f, &fakeAuth{email: "member@example.com"})
	owner := decodeBody[shareResponse](t, do(t, h, http.MethodPost,
		"/v1/sessions/own-1/share", `{"anyone":true}`)).Share

	for _, w := range []*httptest.ResponseRecorder{
		do(t, h, http.MethodGet, "/v1/sessions/own-1", ""),
		do(t, h, http.MethodGet, "/v1/sessions/no-such-session", ""),
		do(t, h, http.MethodDelete, "/v1/shares/"+owner.ID, ""),
	} {
		// A shared cache holding one of these would serve a colleague's
		// transcript to whoever asked next, with no permission check and no
		// audit row.
		if got := w.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
			t.Fatalf("Cache-Control = %q on a %d", got, w.Code)
		}
		if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Fatalf("X-Content-Type-Options = %q", got)
		}
	}
}

func TestWrongMethodSaysNothingAboutASession(t *testing.T) {
	f := seeded()
	h := newHandler(t, f, &fakeAuth{email: "member@example.com"})

	// The route exists whatever the session id is, so the answer is the same
	// for one that exists, one that does not, and one belonging to somebody
	// else. Nothing is revealed by it.
	var codes []int
	for _, id := range []string{"own-1", "other-1", "no-such-session"} {
		codes = append(codes, do(t, h, http.MethodDelete, "/v1/sessions/"+id, "").Code)
	}
	for _, c := range codes {
		if c != codes[0] {
			t.Fatalf("method rejection varies by session: %v", codes)
		}
	}
	// The fallback absorbs it, so a verb this route does not serve reads as a
	// route that does not exist.
	if codes[0] != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", codes[0])
	}
}

func TestNewRequiresItsDependencies(t *testing.T) {
	if _, err := New(Options{Auth: &fakeAuth{}}); err == nil {
		t.Fatal("New without a store should fail")
	}
	if _, err := New(Options{Store: newFakeStore()}); err == nil {
		t.Fatal("New without an authenticator should fail")
	}
}
