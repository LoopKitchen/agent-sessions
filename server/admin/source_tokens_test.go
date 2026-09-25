package admin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The token routes against the fake store: who may call them, what a
// refused request never reaches, what a mint returns and audits, and that
// the change and its audit row are one transaction.

const tokenID = "12345678-1234-4123-8123-123456789abc"

// fakeToken is one source token as the fake store holds it.
type fakeToken struct {
	ID, Platform, Environment, Scope, Label string
	Days                                    int
	Origins                                 []string
	Limit                                   int
	Revoked                                 bool
	RevokedBy                               string
}

func cloneTokens(m map[string]fakeToken) map[string]fakeToken {
	out := make(map[string]fakeToken, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (t fakeTx) MintSourceToken(_ context.Context, actor string, req SourceTokenMint) (SourceTokenIssued, error) {
	if t.s.errToken != nil {
		return SourceTokenIssued{}, t.s.errToken
	}
	// The fake refuses what the store's normaliser would, on one field.
	switch {
	case req.Platform != "devin" && req.Platform != "capy" && req.Platform != "claude_code" && req.Platform != "codex" && req.Platform != "vorflux":
		return SourceTokenIssued{}, &FieldError{Field: "platform", Reason: "must be a platform"}
	case req.Environment == "":
		return SourceTokenIssued{}, &FieldError{Field: "environment", Reason: "must match the shape"}
	}
	scope := req.Scope
	if scope == "" {
		scope = "skill-invocations"
	}
	if t.s.tokens == nil {
		t.s.tokens = map[string]fakeToken{}
	}
	id := tokenID
	if len(t.s.tokens) > 0 {
		id = "22222222-2222-4222-8222-222222222222"
	}
	t.s.tokens[id] = fakeToken{ID: id, Platform: req.Platform, Environment: req.Environment, Scope: scope, Label: req.Label,
		Days: req.ExpiresInDays, Origins: req.AllowedOrigins, Limit: 1200}
	return SourceTokenIssued{
		ID: id, Token: "lss_" + strings.Repeat("x", 43), Platform: req.Platform, Environment: req.Environment, Scope: scope,
		Label: req.Label, ExpiresAt: testNow.Add(time.Duration(req.ExpiresInDays) * 24 * time.Hour), RateLimitPerMin: 1200,
		AllowedOrigins: req.AllowedOrigins,
	}, nil
}

func (t fakeTx) SetSourceTokenLimit(_ context.Context, id string, perMin int) error {
	if t.s.errToken != nil {
		return t.s.errToken
	}
	tok, ok := t.s.tokens[id]
	if !ok {
		return ErrTokenNotFound
	}
	tok.Limit = perMin
	t.s.tokens[id] = tok
	return nil
}

func (t fakeTx) RevokeSourceToken(_ context.Context, id, actor string) error {
	if t.s.errToken != nil {
		return t.s.errToken
	}
	tok, ok := t.s.tokens[id]
	if !ok {
		return ErrTokenNotFound
	}
	tok.Revoked, tok.RevokedBy = true, actor
	t.s.tokens[id] = tok
	return nil
}

func withToken(s *fakeStore) *fakeStore {
	s.tokens = map[string]fakeToken{tokenID: {ID: tokenID, Platform: "devin", Environment: "default", Scope: "skill-invocations", Limit: 1200}}
	return s
}

// tokenPost sends a cookie POST as the named user with the browser's
// same-origin mark unless the test says otherwise.
func tokenPost(t *testing.T, h *Handler, path, user, body, fetchSite string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(http.MethodPost, path, nil)
	} else {
		r = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	}
	if user != "" {
		r.Header.Set("X-Test-User", user)
	}
	if fetchSite != "" {
		r.Header.Set("Sec-Fetch-Site", fetchSite)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

const (
	mintPath   = "/v1/admin/source-tokens"
	limitPath  = "/v1/admin/source-tokens/" + tokenID + "/limit"
	revokePath = "/v1/admin/source-tokens/" + tokenID + "/revoke"
	goodMint   = `{"platform":"devin","environment":"default","label":"devin org","expires_in_days":150}`
)

func TestTokenRoutesRefuseACrossSitePOSTBeforeAnythingIsRead(t *testing.T) {
	for _, site := range []string{"cross-site", "same-site", "none", ""} {
		for _, rt := range []struct{ path, body string }{{mintPath, goodMint}, {limitPath, `{"rate_limit_per_min":0}`}, {revokePath, ""}} {
			s := withToken(newStore(admin("boss@example.com")))
			w := tokenPost(t, newHandler(t, s), rt.path, "boss@example.com", rt.body, site)
			if w.Code != http.StatusForbidden {
				t.Errorf("%s with Sec-Fetch-Site %q: status = %d, want 403: %s", rt.path, site, w.Code, w.Body.String())
			}
			if len(s.actions) != 0 || s.tokens[tokenID].Limit != 1200 || s.tokens[tokenID].Revoked || len(s.tokens) != 1 {
				t.Errorf("%s with Sec-Fetch-Site %q: a refused POST reached the store", rt.path, site)
			}
		}
	}
	// A member on a cross-site POST still sees the admin routes' 404; the
	// no-identity and member cases of every route are covered through
	// adminRoutes (handler_test.go), which lists the three token routes.
	s := withToken(newStore(admin("boss@example.com"), member("dev@example.com")))
	if w := tokenPost(t, newHandler(t, s), revokePath, "dev@example.com", "", "cross-site"); w.Code != http.StatusNotFound {
		t.Errorf("member cross-site revoke: status = %d, want 404", w.Code)
	}
}

func TestMintSourceTokenReturnsThePlaintextOnceAndAudits(t *testing.T) {
	s := newStore(admin("boss@example.com"))
	h := newHandler(t, s)
	w := tokenPost(t, h, mintPath, "boss@example.com", goodMint, "same-origin")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	res := decode[mintResponse](t, w)
	if res.ID != tokenID || !strings.HasPrefix(res.Token, "lss_") || res.Platform != "devin" || res.Environment != "default" ||
		res.Scope != "skill-invocations" || res.Label != "devin org" || res.RateLimitPerMin != 1200 ||
		len(res.AllowedOrigins) != 1 || res.AllowedOrigins[0] != "beacon" || !res.ExpiresAt.Equal(testNow.Add(150*24*time.Hour)) {
		t.Errorf("response = %+v", res)
	}
	tok := s.tokens[tokenID]
	if tok.Days != 150 || len(tok.Origins) != 1 || tok.Origins[0] != "beacon" {
		t.Errorf("row = %+v; allowed_origins defaults to beacon", tok)
	}
	if len(s.actions) != 1 {
		t.Fatalf("%d audit rows, want 1", len(s.actions))
	}
	a := s.actions[0]
	if a.Actor != "boss@example.com" || a.Action != "source_token.mint" || a.Target != tokenID {
		t.Errorf("audit row = %+v", a)
	}
	d, _ := a.Detail.(map[string]any)
	if d == nil || d["platform"] != "devin" || d["expires_in_days"] != 150 || d["label"] != "devin org" {
		t.Errorf("audit detail = %v", a.Detail)
	}
	for k, v := range d {
		if str, ok := v.(string); ok && strings.HasPrefix(str, "lss_") {
			t.Errorf("the audit detail carries the plaintext under %s", k)
		}
	}

	// Explicit origins and a catalog scope pass through.
	s = newStore(admin("boss@example.com"))
	w = tokenPost(t, newHandler(t, s), mintPath, "boss@example.com",
		`{"platform":"claude_code","environment":"catalog-backend","scope":"skill-catalog","expires_in_days":1,"allowed_origins":["hook","reconciler"]}`, "same-origin")
	if w.Code != http.StatusOK {
		t.Fatalf("catalog mint: status = %d: %s", w.Code, w.Body.String())
	}
	if tok := s.tokens[tokenID]; tok.Scope != "skill-catalog" || len(tok.Origins) != 2 {
		t.Errorf("catalog row = %+v", tok)
	}
}

func TestMintSourceTokenRefusals(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		status int
		field  string
	}{
		{"expires_in_days missing", `{"platform":"devin","environment":"default"}`, 400, "expires_in_days"},
		{"expires_in_days zero", `{"platform":"devin","environment":"default","expires_in_days":0}`, 400, "expires_in_days"},
		{"expires_in_days 181", `{"platform":"devin","environment":"default","expires_in_days":181}`, 400, "expires_in_days"},
		{"expires_in_days 400", `{"platform":"devin","environment":"default","expires_in_days":400}`, 400, "expires_in_days"},
		{"laptop environment", `{"platform":"claude_code","environment":"laptop","expires_in_days":30}`, 403, ""},
		{"bound_actor_email", `{"platform":"claude_code","environment":"cloud","expires_in_days":30,"bound_actor_email":"dev@example.com"}`, 400, "bound_actor_email"},
		{"platform the store refuses", `{"platform":"github","environment":"default","expires_in_days":30}`, 400, "platform"},
		{"unknown key", `{"platform":"devin","environment":"default","expires_in_days":30,"rate_limit_per_min":5}`, 400, ""},
		{"malformed", `{"platform":`, 400, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newStore(admin("boss@example.com"))
			w := tokenPost(t, newHandler(t, s), mintPath, "boss@example.com", c.body, "same-origin")
			if w.Code != c.status {
				t.Fatalf("status = %d, want %d: %s", w.Code, c.status, w.Body.String())
			}
			if c.field != "" && !strings.Contains(w.Body.String(), `"field":"`+c.field+`"`) {
				t.Errorf("body names no field %s: %s", c.field, w.Body.String())
			}
			if len(s.tokens) != 0 || len(s.actions) != 0 {
				t.Errorf("a refused mint reached the store: tokens %v actions %v", s.tokens, s.actions)
			}
		})
	}
}

func TestLimitAndRevokeChangeTheRowAndAudit(t *testing.T) {
	s := withToken(newStore(admin("boss@example.com")))
	h := newHandler(t, s)

	w := tokenPost(t, h, limitPath, "boss@example.com", `{"rate_limit_per_min":0}`, "same-origin")
	if w.Code != http.StatusOK {
		t.Fatalf("limit: status = %d: %s", w.Code, w.Body.String())
	}
	if res := decode[limitResponse](t, w); res.ID != tokenID || res.RateLimitPerMin != 0 {
		t.Errorf("limit response = %+v", res)
	}
	if s.tokens[tokenID].Limit != 0 {
		t.Errorf("limit not written: %+v", s.tokens[tokenID])
	}
	if len(s.actions) != 1 || s.actions[0].Action != "source_token.limit" || s.actions[0].Target != tokenID {
		t.Errorf("audit rows = %+v", s.actions)
	}
	if d, _ := s.actions[0].Detail.(map[string]any); d["rate_limit_per_min"] != 0 {
		t.Errorf("audit detail = %v", s.actions[0].Detail)
	}

	for _, body := range []string{`{"rate_limit_per_min":100001}`, `{"rate_limit_per_min":-1}`, `{}`, `{"limit":5}`} {
		if w := tokenPost(t, h, limitPath, "boss@example.com", body, "same-origin"); w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", body, w.Code)
		}
	}
	if w := tokenPost(t, h, "/v1/admin/source-tokens/99999999-9999-4999-8999-999999999999/limit", "boss@example.com", `{"rate_limit_per_min":5}`, "same-origin"); w.Code != http.StatusNotFound {
		t.Errorf("unknown id: status = %d, want 404", w.Code)
	}

	w = tokenPost(t, h, revokePath, "boss@example.com", "", "same-origin")
	if w.Code != http.StatusOK {
		t.Fatalf("revoke: status = %d: %s", w.Code, w.Body.String())
	}
	if res := decode[revokeResponse](t, w); res.ID != tokenID || !res.Revoked {
		t.Errorf("revoke response = %+v", res)
	}
	if tok := s.tokens[tokenID]; !tok.Revoked || tok.RevokedBy != "boss@example.com" {
		t.Errorf("revoke not written: %+v", tok)
	}
	if len(s.actions) != 2 || s.actions[1].Action != "source_token.revoke" || s.actions[1].Target != tokenID {
		t.Errorf("audit rows = %+v", s.actions)
	}
	if w := tokenPost(t, h, "/v1/admin/source-tokens/99999999-9999-4999-8999-999999999999/revoke", "boss@example.com", "", "same-origin"); w.Code != http.StatusNotFound {
		t.Errorf("unknown id: status = %d, want 404", w.Code)
	}
}

// The audit row and the change are one transaction: a failing audit write
// rolls the limit change back, and a demotion landing inside the
// transaction stops the change.
func TestTokenAuditFailureAndMidRequestDemotionRollBack(t *testing.T) {
	s := withToken(newStore(admin("boss@example.com")))
	s.errAction = errors.New("audit table gone")
	if w := tokenPost(t, newHandler(t, s), limitPath, "boss@example.com", `{"rate_limit_per_min":0}`, "same-origin"); w.Code != http.StatusInternalServerError {
		t.Errorf("audit failure: status = %d, want 500", w.Code)
	}
	if s.tokens[tokenID].Limit != 1200 {
		t.Errorf("the limit change survived a failed audit write: %+v", s.tokens[tokenID])
	}
	if w := tokenPost(t, newHandler(t, s), mintPath, "boss@example.com", goodMint, "same-origin"); w.Code != http.StatusInternalServerError || len(s.tokens) != 1 {
		t.Errorf("mint under a failed audit: status = %d, tokens %d", w.Code, len(s.tokens))
	}

	s = withToken(newStore(admin("boss@example.com"), admin("other@example.com")))
	s.onTx = func(s *fakeStore) {
		p := s.principals["boss@example.com"]
		p.Role = RoleMember
		s.principals["boss@example.com"] = p
	}
	if w := tokenPost(t, newHandler(t, s), revokePath, "boss@example.com", "", "same-origin"); w.Code != http.StatusNotFound {
		t.Errorf("demoted mid-request: status = %d, want 404", w.Code)
	}
	if s.tokens[tokenID].Revoked || len(s.actions) != 0 {
		t.Error("a demoted caller's revoke ran")
	}
}
