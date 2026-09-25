package slack

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
)

// The preference routes, under /v1/ so they sit with the rest of the JSON API
// and inherit its 404 for anything unmatched.
//
// There is no HTML control for this yet: server/web renders the dashboard and
// is somebody else's file. That is a real gap in the product and it is named
// here rather than left to be discovered — until a page exists, a person turns
// their own mirror on with one authenticated PUT, and everything below is what
// makes that safe to expose on its own.
const (
	PrefsPath = "/v1/slack/preferences"

	// maxPrefsBody bounds the request. Three short fields, so anything larger is
	// either a mistake or an attempt to make the decoder do work; both are
	// answered the same way.
	maxPrefsBody = 4 << 10
)

// Register mounts the preference routes on a mux owned by the caller.
//
// Absolute patterns, matching every other package here, so a route cannot be
// reachable through one mount and missing from another. The server mounts this
// on the same mux as the read API, whose "/v1/" fallback then absorbs any verb
// these two patterns do not serve.
func (m *Mirror) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET "+PrefsPath, m.handleGetPrefs)
	mux.HandleFunc("PUT "+PrefsPath, m.handlePutPrefs)
	mux.HandleFunc("POST /v1/sessions/{id}/mirror", m.handleSessionMirror)
}

// handleSessionMirror is the mid-session opt-in: mark one session as asked,
// and the next live pass opens its thread, back-posting from what is already
// captured. Owner-only by construction — the UPDATE is keyed on the caller's
// own email, so somebody else's session id changes zero rows and the response
// says so. Where it posts is still the owner's standing preference; this
// route grants nothing an off mode refuses.
func (m *Mirror) handleSessionMirror(w http.ResponseWriter, r *http.Request) {
	email, ok := m.caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	tag, err := m.db.Exec(r.Context(), `
		UPDATE sessions SET mirror_request = 'on', updated_at = now()
		 WHERE session_id = $1 AND email = $2 AND mirror_request = ''`, id, email)
	if err != nil {
		m.failPrefs(w, r, "mark the session for mirroring", err)
		return
	}
	var already bool
	if tag.RowsAffected() == 0 {
		// Idempotent for the session already marked; absent for one that is
		// not the caller's, indistinguishably from one that does not exist.
		err := m.db.QueryRow(r.Context(), `
			SELECT mirror_request <> '' FROM sessions
			 WHERE session_id = $1 AND email = $2`, id, email).Scan(&already)
		if err != nil || !already {
			http.Error(w, "no such session of yours", http.StatusNotFound)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, "{\"session_id\":%q,\"mirror\":\"on\"}\n", id)
}

// prefsBody is what a person may say about their own mirror.
//
// There is deliberately no email field. The address is taken from the session
// on every request and from nowhere else, which is the whole security argument
// for this route: a body that could name a subject would let anybody who can
// sign in start mirroring a colleague's work into a channel of their choosing,
// and the first anybody would know of it is the messages arriving. Decoding
// rejects unknown fields, so a client that sends one is told, rather than
// having it silently ignored and believing it worked.
type prefsBody struct {
	Mode    string `json:"mode"`
	Channel string `json:"channel"`
}

// handleGetPrefs reports the caller's own preference.
//
// Somebody who has never touched this gets the off default rather than a 404,
// because "no row" and "off" are the same state and a client that had to tell
// them apart would be one more place the default could be got wrong.
func (m *Mirror) handleGetPrefs(w http.ResponseWriter, r *http.Request) {
	email, ok := m.caller(w, r)
	if !ok {
		return
	}
	p, err := readPrefs(r.Context(), m.db, email)
	if err != nil {
		m.failPrefs(w, r, "read preferences", err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// handlePutPrefs sets the caller's own preference.
//
// A DM resolves the person's Slack account here rather than at post time, and
// that is the one thing this route does beyond a write. users.lookupByEmail is
// the only call in this feature that fails for a reason the person can act on —
// their Slack account carries a different address from their Workspace one —
// and a lookup deferred to the poster reports that into a log nobody reads,
// leaving somebody who just switched their mirror on watching for messages that
// will never arrive.
func (m *Mirror) handlePutPrefs(w http.ResponseWriter, r *http.Request) {
	email, ok := m.caller(w, r)
	if !ok {
		return
	}
	body, ok := m.decodePrefs(w, r)
	if !ok {
		return
	}

	mode := Mode(strings.ToLower(strings.TrimSpace(body.Mode)))
	if !mode.Valid() {
		writeError(w, http.StatusBadRequest, "invalid_mode",
			fmt.Sprintf("mode must be one of %q, %q or %q", ModeOff, ModeDM, ModeChannel))
		return
	}
	channel := strings.TrimSpace(body.Channel)
	if mode == ModeChannel && channel == "" {
		// Refused rather than stored, because the database refuses it too and a
		// constraint violation would reach the person as a 500 that says nothing
		// about what they got wrong.
		writeError(w, http.StatusBadRequest, "channel_required",
			"channel mode needs a channel to post in")
		return
	}

	p := Prefs{Email: email, Mode: mode, Channel: channel}
	if mode == ModeDM {
		userID, err := m.slack.LookupUserByEmail(r.Context(), email)
		if err != nil {
			if errors.Is(err, ErrUserNotFound) {
				// The one failure here that is the person's to fix, so it says so
				// plainly and names the address it looked for. Nothing about a
				// colleague is disclosed: this is their own address, echoed back.
				writeError(w, http.StatusUnprocessableEntity, "slack_user_not_found",
					fmt.Sprintf("no Slack account in this workspace carries %s, so a DM cannot be opened", email))
				return
			}
			m.failPrefs(w, r, "look up a slack account", err)
			return
		}
		p.SlackUserID = userID
	}

	if err := savePrefs(r.Context(), m.db, p, m.now()); err != nil {
		m.failPrefs(w, r, "save preferences", err)
		return
	}
	// Read back rather than echoed, so the response carries the watermark the
	// database actually applied. A client that trusted the request it just sent
	// would report a mirror_from this server never stored.
	saved, err := readPrefs(r.Context(), m.db, email)
	if err != nil {
		m.failPrefs(w, r, "read back the saved preferences", err)
		return
	}
	m.log.Info("slack mirror preference set", "email", email, "mode", string(saved.Mode))
	writeJSON(w, http.StatusOK, saved)
}

// caller resolves who is asking, or writes the refusal and reports that the
// request is over.
//
// The viewer function is the same one the dashboard uses, so a disabled or
// unenrolled account is already nobody by the time it gets here.
func (m *Mirror) caller(w http.ResponseWriter, r *http.Request) (string, bool) {
	email, ok := m.viewer(r)
	if !ok || strings.TrimSpace(email) == "" {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "sign in to continue")
		return "", false
	}
	return strings.ToLower(strings.TrimSpace(email)), true
}

// decodePrefs reads the body, or writes the refusal and reports that the
// request is over.
//
// The content type is checked rather than assumed, and that check is load
// bearing. A cross-site form POST is a "simple request" a browser will send
// with the session cookie attached and no preflight; requiring JSON — which a
// form cannot produce — means the only way to reach this handler from another
// origin is a preflight this server never answers. PUT is already outside the
// simple set, so this is the second of two independent reasons a hostile page
// cannot switch somebody's mirror on, and it stays correct if a later change
// adds POST.
func (m *Mirror) decodePrefs(w http.ResponseWriter, r *http.Request) (prefsBody, bool) {
	ct, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || ct != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type",
			"send application/json")
		return prefsBody{}, false
	}

	dec := json.NewDecoder(io.LimitReader(r.Body, maxPrefsBody))
	// An unknown field is rejected, not ignored. The field somebody would try is
	// "email", and silently dropping it would let a caller believe they had set
	// a colleague's preference while this server quietly set their own.
	dec.DisallowUnknownFields()
	var body prefsBody
	if err := dec.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "the request body is not the expected JSON")
		return prefsBody{}, false
	}
	return body, true
}

// failPrefs logs the cause and answers with none of it.
//
// The text of a pgx error names hosts, users and constraints, and Slack's names
// a workspace; this endpoint is reachable from the internet by anybody with an
// account. What went wrong belongs in the log with the path that produced it.
func (m *Mirror) failPrefs(w http.ResponseWriter, r *http.Request, what string, err error) {
	m.log.Error("slack preferences: "+what, "err", err, "path", r.URL.Path)
	writeError(w, http.StatusInternalServerError, "internal", "internal error")
}

// ---------------------------------------------------------------------------
// Responses
// ---------------------------------------------------------------------------

// errorBody is the shape server/api answers with. Restated here rather than
// imported because these two packages share a URL prefix and nothing else, and
// a client parsing /v1/ should not have to know which of them replied.
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, errorBody{Error: errorDetail{Code: code, Message: msg}})
}

// writeJSON encodes into a buffer before touching the ResponseWriter, because
// encoding straight to it commits the status line first and a marshal failure
// halfway through would append error text to a body already declared a success.
func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	// Never cached. This is one person's setting answered on a shared path, and
	// a cache that kept a copy would serve it to whoever asked next.
	h.Set("Cache-Control", "no-store")
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"internal","message":"internal error"}}`))
		return
	}
	w.WriteHeader(status)
	_, _ = w.Write(b)
}
