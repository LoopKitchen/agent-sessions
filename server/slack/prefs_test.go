package slack

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// prefsServer assembles the route the way the composition root does: on a mux,
// through Register, with a viewer that reports whoever the test says is signed
// in. Nothing here calls a handler directly, so a route that was never mounted
// fails these tests rather than passing them.
func prefsServer(t *testing.T, db DB, poster Poster, who string) (http.Handler, *Mirror) {
	t.Helper()
	m := testMirror(t, db, poster, func(o *Options) {
		o.Viewer = func(*http.Request) (string, bool) {
			if who == "" {
				return "", false
			}
			return who, true
		}
	})
	mux := http.NewServeMux()
	m.Register(mux)
	return mux, m
}

func putPrefs(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, PrefsPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestNobodyCanSetAPreferenceForAnybodyElse is the security property of this
// route, and the reason prefsBody has no email field.
//
// The subject is taken from the session and from nowhere else. If a body could
// name one, anybody who can sign in could start mirroring a colleague's work
// into a channel of their choosing, and the first anybody would know of it is
// the messages arriving in it.
func TestNobodyCanSetAPreferenceForAnybodyElse(t *testing.T) {
	db := newFakeDB()
	fs := newFakeSlack(t)
	h, _ := prefsServer(t, db, fs.client(t), "ana@example.org")

	// A body naming somebody else is refused outright rather than quietly
	// applied to the caller's own row: a caller who believed it worked would
	// have no reason to look again.
	rec := putPrefs(t, h, `{"mode":"channel","channel":"C999","email":"bo@example.org"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a body naming another person answered %d, want 400", rec.Code)
	}

	db.mu.Lock()
	defer db.mu.Unlock()
	if _, wrote := db.prefs["bo@example.org"]; wrote {
		t.Fatal("a preference was written for somebody who did not ask for it")
	}
	if _, wrote := db.prefs["ana@example.org"]; wrote {
		t.Error("a rejected request still wrote the caller's own preference")
	}
}

// TestThePreferenceRouteRefusesARequestWithNoSession. This writes a setting
// that causes somebody's work to be posted publicly; an unauthenticated caller
// must not reach the database at all.
func TestThePreferenceRouteRefusesARequestWithNoSession(t *testing.T) {
	tests := []struct {
		name   string
		method string
		body   string
	}{
		{"reading somebody's preference", http.MethodGet, ""},
		{"setting somebody's preference", http.MethodPut, `{"mode":"dm"}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := newFakeDB()
			fs := newFakeSlack(t)
			h, _ := prefsServer(t, db, fs.client(t), "")

			req := httptest.NewRequest(tc.method, PrefsPath, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("answered %d, want 401", rec.Code)
			}
			db.mu.Lock()
			defer db.mu.Unlock()
			if len(db.statements) != 0 {
				t.Errorf("an unauthenticated request issued %d statements, want none", len(db.statements))
			}
		})
	}
}

// TestSomebodyWhoHasNeverHeardOfThisFeatureIsOff. Absence and "off" are the
// same state, and a client that had to tell them apart would be one more place
// the privacy default could be got wrong.
func TestSomebodyWhoHasNeverHeardOfThisFeatureIsOff(t *testing.T) {
	db := newFakeDB()
	fs := newFakeSlack(t)
	h, _ := prefsServer(t, db, fs.client(t), "ana@example.org")

	req := httptest.NewRequest(http.MethodGet, PrefsPath, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("answered %d, want 200", rec.Code)
	}
	var got Prefs
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Mode != ModeOff {
		t.Errorf("mode = %q for somebody with no row, want %q", got.Mode, ModeOff)
	}
	if !got.Off() {
		t.Error("Off() disagrees with the mode it was built from")
	}
}

// TestSettingAPreferenceStoresItAgainstTheSignedInPersonOnly.
func TestSettingAPreferenceStoresItAgainstTheSignedInPersonOnly(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		wantMode    Mode
		wantChannel string
		wantLookup  bool
	}{
		{
			name:       "a DM resolves the Slack account while the person is still watching",
			body:       `{"mode":"dm"}`,
			wantMode:   ModeDM,
			wantLookup: true,
		},
		{
			name:        "a channel is stored as given and needs no account lookup",
			body:        `{"mode":"channel","channel":"C42"}`,
			wantMode:    ModeChannel,
			wantChannel: "C42",
		},
		{
			name:     "turning it off is always allowed and asks Slack nothing",
			body:     `{"mode":"off"}`,
			wantMode: ModeOff,
		},
		{
			name:     "the mode is read case-insensitively, because a person may type it",
			body:     `{"mode":"DM"}`,
			wantMode: ModeDM, wantLookup: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := newFakeDB()
			fs := newFakeSlack(t)
			h, _ := prefsServer(t, db, fs.client(t), "ana@example.org")

			rec := putPrefs(t, h, tc.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("answered %d: %s", rec.Code, rec.Body)
			}

			db.mu.Lock()
			saved := db.prefs["ana@example.org"]
			db.mu.Unlock()
			if saved.Mode != tc.wantMode {
				t.Errorf("stored mode %q, want %q", saved.Mode, tc.wantMode)
			}
			if saved.Channel != tc.wantChannel {
				t.Errorf("stored channel %q, want %q", saved.Channel, tc.wantChannel)
			}
			if (saved.SlackUserID != "") != tc.wantLookup {
				t.Errorf("slack id %q, want resolved=%v", saved.SlackUserID, tc.wantLookup)
			}

			var looked bool
			for _, c := range fs.snapshot() {
				if c.Method == "users.lookupByEmail" {
					looked = true
				}
			}
			if looked != tc.wantLookup {
				t.Errorf("looked up an account: %v, want %v", looked, tc.wantLookup)
			}
			// Nothing is posted by setting a preference. A confirmation message
			// would be the mirror announcing itself in a channel before anybody
			// had finished configuring it.
			if got := len(fs.posts()); got != 0 {
				t.Errorf("setting a preference posted %d messages", got)
			}
		})
	}
}

// TestAPreferenceThatCouldNeverPostIsRefusedAtTheEdge. Each of these would
// otherwise be stored and then discovered by the poster, which reports it into
// a log while the person waits for messages that cannot come.
func TestAPreferenceThatCouldNeverPostIsRefusedAtTheEdge(t *testing.T) {
	tests := []struct {
		name string
		body string
		want int
		code string
	}{
		{"channel mode with no channel", `{"mode":"channel"}`, http.StatusBadRequest, "channel_required"},
		{"channel mode with only whitespace", `{"mode":"channel","channel":"   "}`, http.StatusBadRequest, "channel_required"},
		{"a mode nothing implements", `{"mode":"carrier-pigeon"}`, http.StatusBadRequest, "invalid_mode"},
		{"no mode at all", `{}`, http.StatusBadRequest, "invalid_mode"},
		{"a body that is not JSON", `not json`, http.StatusBadRequest, "invalid_body"},
		{"a field this route does not define", `{"mode":"dm","admin":true}`, http.StatusBadRequest, "invalid_body"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := newFakeDB()
			fs := newFakeSlack(t)
			h, _ := prefsServer(t, db, fs.client(t), "ana@example.org")

			rec := putPrefs(t, h, tc.body)
			if rec.Code != tc.want {
				t.Fatalf("answered %d, want %d: %s", rec.Code, tc.want, rec.Body)
			}
			var body errorBody
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("the failure is not the JSON error shape: %v", err)
			}
			if body.Error.Code != tc.code {
				t.Errorf("error code %q, want %q", body.Error.Code, tc.code)
			}
			db.mu.Lock()
			defer db.mu.Unlock()
			if len(db.prefs) != 0 {
				t.Error("a refused preference was stored anyway")
			}
		})
	}
}

// TestOnlyJSONIsAccepted, which is what makes this route unreachable from
// another origin.
//
// A cross-site form POST is a "simple request": the browser sends it with the
// session cookie attached and no preflight. A form cannot produce
// application/json, so requiring it means the only way here from another origin
// is a preflight this server never answers. PUT is already outside the simple
// set, so this is the second of two independent reasons, and it is the one that
// stays correct if somebody later adds POST.
func TestOnlyJSONIsAccepted(t *testing.T) {
	for _, ct := range []string{"application/x-www-form-urlencoded", "text/plain", "multipart/form-data", ""} {
		t.Run("content type "+ct, func(t *testing.T) {
			db := newFakeDB()
			fs := newFakeSlack(t)
			h, _ := prefsServer(t, db, fs.client(t), "ana@example.org")

			req := httptest.NewRequest(http.MethodPut, PrefsPath, strings.NewReader(`{"mode":"dm"}`))
			if ct != "" {
				req.Header.Set("Content-Type", ct)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnsupportedMediaType {
				t.Fatalf("answered %d, want 415", rec.Code)
			}
			db.mu.Lock()
			defer db.mu.Unlock()
			if len(db.prefs) != 0 {
				t.Error("a request this route should not accept still wrote a preference")
			}
		})
	}
}

// TestAJSONContentTypeWithACharsetIsStillJSON. Browsers and clients append one
// routinely, and rejecting it would make the route fail for the callers most
// likely to be correct.
func TestAJSONContentTypeWithACharsetIsStillJSON(t *testing.T) {
	db := newFakeDB()
	fs := newFakeSlack(t)
	h, _ := prefsServer(t, db, fs.client(t), "ana@example.org")

	req := httptest.NewRequest(http.MethodPut, PrefsPath, strings.NewReader(`{"mode":"off"}`))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("answered %d, want 200: %s", rec.Code, rec.Body)
	}
}

// TestAnAddressSlackCannotFindIsReportedToThePersonWhoJustAskedForIt. The
// alternative is a preference that stores cleanly and never produces a message,
// with the reason in a server log.
func TestAnAddressSlackCannotFindIsReportedToThePersonWhoJustAskedForIt(t *testing.T) {
	db := newFakeDB()
	fs := newFakeSlack(t)
	fs.set(func(f *fakeSlack) { f.lookupErr = "users_not_found" })
	h, _ := prefsServer(t, db, fs.client(t), "ghost@example.org")

	rec := putPrefs(t, h, `{"mode":"dm"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("answered %d, want 422: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "ghost@example.org") {
		t.Errorf("the answer does not name the address that could not be found: %s", rec.Body)
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	if len(db.prefs) != 0 {
		t.Error("a DM preference was stored for an address Slack does not know")
	}
}

// TestADatabaseFailureSaysNothingAboutTheDatabase. A pgx error names hosts,
// users and constraints, and this endpoint is reachable by anybody with an
// account.
func TestADatabaseFailureSaysNothingAboutTheDatabase(t *testing.T) {
	db := newFakeDB()
	db.failOn = "slack_prefs"
	fs := newFakeSlack(t)
	h, _ := prefsServer(t, db, fs.client(t), "ana@example.org")

	req := httptest.NewRequest(http.MethodGet, PrefsPath, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("answered %d, want 500", rec.Code)
	}
	for _, leak := range []string{"slack_prefs", "refused", "fakeDB"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Errorf("the answer leaks %q: %s", leak, rec.Body)
		}
	}
}

// TestAPreferenceIsNeverCached. It is one person's setting answered on a path
// everybody shares, and a cache that kept a copy would hand it to whoever asked
// next.
func TestAPreferenceIsNeverCached(t *testing.T) {
	db := newFakeDB()
	fs := newFakeSlack(t)
	h, _ := prefsServer(t, db, fs.client(t), "ana@example.org")

	req := httptest.NewRequest(http.MethodGet, PrefsPath, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q, want JSON", got)
	}
}

// TestAnOversizedBodyIsNotRead. Three short fields, so anything larger is a
// mistake or an attempt to make the decoder do work.
func TestAnOversizedBodyIsNotRead(t *testing.T) {
	db := newFakeDB()
	fs := newFakeSlack(t)
	h, _ := prefsServer(t, db, fs.client(t), "ana@example.org")

	huge := `{"mode":"channel","channel":"` + strings.Repeat("C", maxPrefsBody*2) + `"}`
	rec := putPrefs(t, h, huge)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("answered %d, want 400", rec.Code)
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	if len(db.prefs) != 0 {
		t.Error("an oversized body still wrote a preference")
	}
}
