package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeMinter records the mint the handler asked for, which is the whole
// property under test: the server, not the body, decides every column but
// the label and the lifetime.
type fakeMinter struct {
	last  *SourceTokenMint
	as    Viewer
	err   error
	calls int
}

func (f *fakeMinter) MintSourceToken(_ context.Context, v Viewer, m SourceTokenMint) (MintedSourceToken, error) {
	f.calls++
	f.as, f.last = v, &m
	if f.err != nil {
		return MintedSourceToken{}, f.err
	}
	return MintedSourceToken{ID: "12345678-1234-4123-8123-123456789abc", Token: "lss_" + strings.Repeat("x", 43),
		ExpiresAt: testNow.Add(time.Duration(m.ExpiresInDays) * 24 * time.Hour)}, nil
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func laptopHandler(t *testing.T, f *fakeStore, email string, m *fakeMinter) *Handler {
	t.Helper()
	h, err := New(Options{
		Store:        f,
		Auth:         &fakeAuth{email: email},
		SourceTokens: m,
		Now:          func() time.Time { return testNow },
		Logger:       quietLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h
}

func laptopPost(t *testing.T, h *Handler, body, fetchSite string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(http.MethodPost, "/v1/source-tokens/laptop", nil)
	} else {
		r = httptest.NewRequest(http.MethodPost, "/v1/source-tokens/laptop", strings.NewReader(body))
	}
	if fetchSite != "" {
		r.Header.Set("Sec-Fetch-Site", fetchSite)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestLaptopMintSetsEveryColumnButTheLabelAndTheLifetime(t *testing.T) {
	m := &fakeMinter{}
	h := laptopHandler(t, seeded(), "member@example.com", m)
	w := laptopPost(t, h, `{"label":"my macbook"}`, "same-origin")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if m.last == nil || m.calls != 1 {
		t.Fatal("the minter was not asked once")
	}
	want := SourceTokenMint{Platform: "claude_code", Environment: "laptop", Scope: "skill-invocations", Label: "my macbook",
		ExpiresInDays: 180, AllowedOrigins: []string{"hook"}, BoundActorEmail: "member@example.com"}
	if m.last.Platform != want.Platform || m.last.Environment != want.Environment || m.last.Scope != want.Scope ||
		m.last.Label != want.Label || m.last.ExpiresInDays != want.ExpiresInDays || len(m.last.AllowedOrigins) != 1 ||
		m.last.AllowedOrigins[0] != "hook" || m.last.BoundActorEmail != want.BoundActorEmail {
		t.Errorf("mint = %+v, want %+v", *m.last, want)
	}
	if m.as.Email != "member@example.com" || m.as.Role != RoleMember {
		t.Errorf("minted as %+v", m.as)
	}
	res := decodeBody[laptopMintResponse](t, w)
	if !strings.HasPrefix(res.Token, "lss_") || res.Platform != "claude_code" || res.Environment != "laptop" ||
		res.Scope != "skill-invocations" || len(res.AllowedOrigins) != 1 || res.AllowedOrigins[0] != "hook" ||
		!res.ExpiresAt.Equal(testNow.Add(180*24*time.Hour)) {
		t.Errorf("response = %+v", res)
	}

	m = &fakeMinter{}
	h = laptopHandler(t, seeded(), "member@example.com", m)
	if w := laptopPost(t, h, `{"expires_in_days":30}`, "same-origin"); w.Code != http.StatusOK || m.last.ExpiresInDays != 30 || m.last.Label != "" {
		t.Errorf("30 days: status %d, mint %+v", w.Code, m.last)
	}
	// No body is {}: the defaults.
	m = &fakeMinter{}
	if w := laptopPost(t, laptopHandler(t, seeded(), "member@example.com", m), "", "same-origin"); w.Code != http.StatusOK || m.last == nil || m.last.ExpiresInDays != 180 {
		t.Errorf("no body: status %d, mint %+v", w.Code, m.last)
	}
}

// A member naming a colleague, or anything else the body may not carry,
// is 400, and the row still binds the viewer when the body is clean.
func TestLaptopMintRefusesEveryKeyButLabelAndLifetime(t *testing.T) {
	cases := []struct {
		name, body string
	}{
		{"a colleague", `{"bound_actor_email":"other@example.com"}`},
		{"actor_email", `{"actor_email":"other@example.com"}`},
		{"an environment", `{"environment":"cloud"}`},
		{"origins", `{"allowed_origins":["beacon"]}`},
		{"a scope", `{"scope":"skill-catalog"}`},
		{"a platform", `{"platform":"devin"}`},
		{"0 days", `{"expires_in_days":0}`},
		{"181 days", `{"expires_in_days":181}`},
		{"400 days", `{"expires_in_days":400}`},
		{"a long label", `{"label":"` + strings.Repeat("x", 81) + `"}`},
		{"malformed", `{"label":`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := &fakeMinter{}
			w := laptopPost(t, laptopHandler(t, seeded(), "member@example.com", m), c.body, "same-origin")
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
			}
			if m.calls != 0 {
				t.Error("a refused body reached the minter")
			}
		})
	}
}

func TestLaptopMintIsACookieRouteWithTheSameOriginCheck(t *testing.T) {
	for _, site := range []string{"cross-site", "same-site", "none", ""} {
		m := &fakeMinter{}
		w := laptopPost(t, laptopHandler(t, seeded(), "member@example.com", m), `{}`, site)
		if w.Code != http.StatusForbidden || m.calls != 0 {
			t.Errorf("Sec-Fetch-Site %q: status = %d (want 403), minter calls %d", site, w.Code, m.calls)
		}
	}
	// No usable cookie: 401, before the same-origin check.
	m := &fakeMinter{}
	h, err := New(Options{Store: seeded(), Auth: &fakeAuth{err: ErrNoIdentity}, SourceTokens: m, Logger: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}
	if w := laptopPost(t, h, `{}`, "same-origin"); w.Code != http.StatusUnauthorized || m.calls != 0 {
		t.Errorf("no identity: status = %d, minter calls %d", w.Code, m.calls)
	}
	// An address with no roster row: the read API's not_enrolled 403.
	if w := laptopPost(t, laptopHandler(t, seeded(), "stranger@example.com", m), `{}`, "same-origin"); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "not_enrolled") {
		t.Errorf("not enrolled: status = %d: %s", w.Code, w.Body.String())
	}
	// A minter failure is a 500, not a token.
	m = &fakeMinter{err: errors.New("disk full")}
	if w := laptopPost(t, laptopHandler(t, seeded(), "member@example.com", m), `{}`, "same-origin"); w.Code != http.StatusInternalServerError {
		t.Errorf("minter failure: status = %d", w.Code)
	}
	// Without the port the route is not mounted and answers the read API's
	// 404.
	h = newHandler(t, seeded(), &fakeAuth{email: "member@example.com"})
	if w := laptopPost(t, h, `{}`, "same-origin"); w.Code != http.StatusNotFound {
		t.Errorf("unmounted: status = %d, want 404", w.Code)
	}
}
