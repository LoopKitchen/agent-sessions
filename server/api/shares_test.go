package api

import (
	"net/http"
	"testing"
	"time"
)

func TestCreateShareRequiresAnExplicitAudience(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"neither", `{}`},
		{"both", `{"grantee":"other@example.com","anyone":true}`},
		{"not an address", `{"grantee":"other"}`},
		{"empty grantee is not a grant to everybody", `{"grantee":""}`},
		{"unknown field", `{"grantie":"other@example.com"}`},
		{"expiry twice", `{"anyone":true,"expires_at":"2026-08-05T00:00:00Z","ttl":"24h"}`},
		{"expiry already past", `{"anyone":true,"expires_at":"2026-08-03T00:00:00Z"}`},
		{"negative lifetime", `{"anyone":true,"ttl":"-1h"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := seeded()
			h := newHandler(t, f, &fakeAuth{email: "member@example.com"})

			w := do(t, h, http.MethodPost, "/v1/sessions/own-1/share", tc.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", w.Code, w.Body)
			}
			if len(f.shares) != 0 {
				t.Fatalf("a share was created anyway: %+v", f.shares)
			}
		})
	}
}

func TestCreateShareNamesItsGrantee(t *testing.T) {
	f := seeded()
	h := newHandler(t, f, &fakeAuth{email: "member@example.com"})

	w := do(t, h, http.MethodPost, "/v1/sessions/own-1/share",
		`{"grantee":"Other@Example.com","ttl":"72h"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%s)", w.Code, w.Body)
	}
	got := decodeBody[shareResponse](t, w).Share
	if got.Grantee != "other@example.com" {
		t.Fatalf("grantee = %q, want it normalised", got.Grantee)
	}
	if got.Token == "" {
		t.Fatal("the link secret is the point of creating a share and was not returned")
	}
	want := testNow.Add(72 * time.Hour)
	if !f.lastShareRequest.ExpiresAt.Equal(want) {
		t.Fatalf("expiry = %v, want %v", f.lastShareRequest.ExpiresAt, want)
	}
}

func TestCreateShareForAnyEmployeeIsAskedForOutright(t *testing.T) {
	f := seeded()
	h := newHandler(t, f, &fakeAuth{email: "member@example.com"})

	w := do(t, h, http.MethodPost, "/v1/sessions/own-1/share", `{"anyone":true}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%s)", w.Code, w.Body)
	}
	if f.lastShareRequest.Grantee != "" {
		t.Fatalf("grantee = %q, want the open grant", f.lastShareRequest.Grantee)
	}
	// No expiry was asked for and none is configured, so the grant is
	// indefinite, exactly as the contract's nullable column says.
	if !f.lastShareRequest.ExpiresAt.IsZero() {
		t.Fatalf("expiry = %v, want none", f.lastShareRequest.ExpiresAt)
	}
}

func TestDefaultShareTTLBoundsAnUnaskedGrant(t *testing.T) {
	f := seeded()
	h, err := New(Options{
		Store:           f,
		Auth:            &fakeAuth{email: "member@example.com"},
		DefaultShareTTL: 24 * time.Hour,
		Now:             func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if w := do(t, h, http.MethodPost, "/v1/sessions/own-1/share", `{"anyone":true}`); w.Code != http.StatusCreated {
		t.Fatalf("status = %d (%s)", w.Code, w.Body)
	}
	if want := testNow.Add(24 * time.Hour); !f.lastShareRequest.ExpiresAt.Equal(want) {
		t.Fatalf("expiry = %v, want the house default of %v", f.lastShareRequest.ExpiresAt, want)
	}
}

func TestSharingSomebodyElsesSessionIsNotFound(t *testing.T) {
	f := seeded()
	member := newHandler(t, f, &fakeAuth{email: "member@example.com"})

	forbidden := do(t, member, http.MethodPost, "/v1/sessions/other-1/share", `{"anyone":true}`)
	absent := do(t, member, http.MethodPost, "/v1/sessions/no-such-session/share", `{"anyone":true}`)
	if forbidden.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s)", forbidden.Code, forbidden.Body)
	}
	if forbidden.Body.String() != absent.Body.String() {
		t.Fatalf("bodies differ:\n %s\n %s", forbidden.Body, absent.Body)
	}

	// An admin may share on somebody's behalf, and the store audits it as a
	// read of their work.
	admin := newHandler(t, f, &fakeAuth{email: "admin@example.com"})
	if w := do(t, admin, http.MethodPost, "/v1/sessions/other-1/share", `{"anyone":true}`); w.Code != http.StatusCreated {
		t.Fatalf("admin status = %d, want 201 (%s)", w.Code, w.Body)
	}
	if len(f.audits) != 1 || f.audits[0].sessionID != "other-1" {
		t.Fatalf("audits = %+v, want the admin's action on the owner's session", f.audits)
	}
}

func TestRevokeShare(t *testing.T) {
	f := seeded()
	h := newHandler(t, f, &fakeAuth{email: "member@example.com"})

	created := decodeBody[shareResponse](t, do(t, h, http.MethodPost,
		"/v1/sessions/own-1/share", `{"grantee":"other@example.com"}`)).Share

	if w := do(t, h, http.MethodDelete, "/v1/shares/"+created.ID, ""); w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (%s)", w.Code, w.Body)
	}
	if w := do(t, h, http.MethodDelete, "/v1/shares/"+created.ID, ""); w.Code != http.StatusNotFound {
		t.Fatalf("second revoke status = %d, want 404", w.Code)
	}
	if w := do(t, h, http.MethodDelete, "/v1/shares/never-existed", ""); w.Code != http.StatusNotFound {
		t.Fatalf("unknown share status = %d, want 404", w.Code)
	}
}

func TestResolveShareGrantsTheSessionAndKeepsTheSecret(t *testing.T) {
	f := seeded()
	owner := newHandler(t, f, &fakeAuth{email: "member@example.com"})
	created := decodeBody[shareResponse](t, do(t, owner, http.MethodPost,
		"/v1/sessions/own-1/share", `{"grantee":"other@example.com"}`)).Share

	guest := newHandler(t, f, &fakeAuth{email: "other@example.com"})
	w := do(t, guest, http.MethodGet, "/v1/shared/"+created.Token, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body)
	}
	body := decodeBody[resolveShareResponse](t, w)
	if body.Session.SessionID != "own-1" {
		t.Fatalf("session = %+v", body.Session)
	}
	// The reader already holds the token; echoing it back only puts the link
	// secret onto a page that any screen share exposes.
	if body.Share.Token != "" {
		t.Fatalf("token echoed back: %q", body.Share.Token)
	}
	if len(f.audits) != 1 || f.audits[0].viewer != "other@example.com" {
		t.Fatalf("audits = %+v, want the share read recorded", f.audits)
	}

	// A revoked link and a link that never existed are the same answer.
	if w := do(t, owner, http.MethodDelete, "/v1/shares/"+created.ID, ""); w.Code != http.StatusNoContent {
		t.Fatalf("revoke status = %d", w.Code)
	}
	revoked := do(t, guest, http.MethodGet, "/v1/shared/"+created.Token, "")
	unknown := do(t, guest, http.MethodGet, "/v1/shared/never-minted", "")
	if revoked.Code != http.StatusNotFound || revoked.Body.String() != unknown.Body.String() {
		t.Fatalf("revoked %d %s vs unknown %d %s", revoked.Code, revoked.Body, unknown.Code, unknown.Body)
	}
}

func TestShareToSomebodyElseDoesNotAdmitTheWrongReader(t *testing.T) {
	f := seeded()
	owner := newHandler(t, f, &fakeAuth{email: "member@example.com"})
	created := decodeBody[shareResponse](t, do(t, owner, http.MethodPost,
		"/v1/sessions/own-1/share", `{"grantee":"other@example.com"}`)).Share

	f.addPrincipal(Principal{Email: "third@example.com", Role: RoleMember})
	third := newHandler(t, f, &fakeAuth{email: "third@example.com"})
	if w := do(t, third, http.MethodGet, "/v1/shared/"+created.Token, ""); w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a link addressed to somebody else", w.Code)
	}
	if w := do(t, third, http.MethodGet, "/v1/sessions/own-1", ""); w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}
