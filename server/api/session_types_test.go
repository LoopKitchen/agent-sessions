package api

import (
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"testing"
	"time"
)

// The types parameter is the one filter whose absence means something. No
// parameter hands the store no list, and the store applies its own default
// view; a parameter passes through as typed, because the store keeps the one
// list of values a type may hold and drops the rest before SQL. The list and
// the search receive the same treatment, or a type the list hides could
// resurface as a transcript hit.
func TestSessionTypesReachTheStoreOnlyWhenNamed(t *testing.T) {
	f := searchStore(t, 1)
	h := newHandler(t, f, &fakeAuth{email: "admin@example.com"})

	if w := do(t, h, http.MethodGet, "/v1/sessions", ""); w.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", w.Code, w.Body)
	}
	if got := f.lastSessionFilter.Types; got != nil {
		t.Errorf("an absent types parameter reached the list as %v, want nil (the store default)", got)
	}
	if w := do(t, h, http.MethodGet, "/v1/sessions?types=user,%20empty,,robot", ""); w.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", w.Code, w.Body)
	}
	if got, want := f.lastSessionFilter.Types, []string{"user", "empty", "robot"}; !reflect.DeepEqual(got, want) {
		t.Errorf("types reached the list as %v, want %v: trimmed, blanks dropped, nothing validated here", got, want)
	}

	if w := do(t, h, http.MethodGet, "/v1/search?q=migration", ""); w.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", w.Code, w.Body)
	}
	if got := f.lastSearchFilter.Types; got != nil {
		t.Errorf("an absent types parameter reached the search as %v, want nil (the store default)", got)
	}
	if w := do(t, h, http.MethodGet, "/v1/search?q=migration&types=internal", ""); w.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", w.Code, w.Body)
	}
	if got, want := f.lastSearchFilter.Types, []string{"internal"}; !reflect.DeepEqual(got, want) {
		t.Errorf("types reached the search as %v, want %v", got, want)
	}
}

// A page cursor names a position in one ordering, and the type selection is
// part of that ordering's identity: the same cursor under another selection,
// or under none, is refused rather than reinterpreted, while the selection it
// was minted under keeps working.
func TestSessionCursorIsBoundToTheTypeSelection(t *testing.T) {
	f := newFakeStore()
	f.addPrincipal(Principal{Email: "member@example.com", Role: RoleMember})
	for i := 1; i <= 4; i++ {
		f.sessions = append(f.sessions, Session{
			SessionID: fmt.Sprintf("s%d", i),
			Email:     "member@example.com",
			Source:    "claude_code",
			Repo:      "loop-sessions",
			StartedAt: testNow.Add(time.Duration(-i) * time.Hour),
		})
	}
	h := newHandler(t, f, &fakeAuth{email: "member@example.com"})

	first := decodeBody[sessionListResponse](t, do(t, h, http.MethodGet, "/v1/sessions?limit=2&types=user,automation", ""))
	if first.NextCursor == "" {
		t.Fatal("expected a next cursor")
	}
	cursor := url.QueryEscape(first.NextCursor)

	for _, target := range []string{
		"/v1/sessions?limit=2&cursor=" + cursor,
		"/v1/sessions?limit=2&types=automation&cursor=" + cursor,
	} {
		w := do(t, h, http.MethodGet, target, "")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400 (%s)", target, w.Code, w.Body)
		}
		if got := decodeBody[errorBody](t, w).Error.Code; got != "invalid_cursor" {
			t.Fatalf("%s: code = %q, want invalid_cursor", target, got)
		}
	}
	if w := do(t, h, http.MethodGet, "/v1/sessions?limit=2&types=user,automation&cursor="+cursor, ""); w.Code != http.StatusOK {
		t.Fatalf("the minting selection: status = %d, want 200 (%s)", w.Code, w.Body)
	}
}
