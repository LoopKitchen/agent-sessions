package slack

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The Slack-side control: buttons on the thread root, answered here.
//
// Feature-gated on the signing secret. Without it this route is not mounted
// and roots carry no buttons, because a button that posts to an endpoint that
// cannot verify who pressed it would let anybody who can forge a request
// detach anybody's mirror — the signature IS the authorization story.
// Activation is two steps on the app's settings page (api.slack.com): paste
// the signing secret into the LOOP_SESSIONS_SLACK_SIGNING_SECRET env, and
// point Interactivity at POST {PublicURL}/v1/slack/interactive.
//
// v1 understands one action: stop_mirror, carried on the root message of a
// thread, valued "sessionID|groupID". Slack verifies the PRESSER is a
// workspace member; this handler additionally requires them to be the
// session's owner, mapped through their Slack user id — a teammate pressing
// Stop on somebody else's thread is answered privately and changes nothing.

// InteractivePath is where the Slack app's Interactivity URL points.
const InteractivePath = "/v1/slack/interactive"

// RegisterInteractive mounts the endpoint when a signing secret exists.
func (m *Mirror) RegisterInteractive(mux *http.ServeMux) {
	if m.signingSecret == "" {
		return
	}
	mux.HandleFunc("POST "+InteractivePath, m.handleInteractive)
}

// Interactive reports whether the Slack-side controls are on, which is what
// decides whether roots carry buttons.
func (m *Mirror) Interactive() bool { return m.signingSecret != "" }

// verifySlackSignature checks Slack's v0 HMAC over the timestamp and body.
// The five-minute window is Slack's own replay bound.
func verifySlackSignature(secret string, r *http.Request, body []byte, now time.Time) bool {
	ts := r.Header.Get("X-Slack-Request-Timestamp")
	sig := r.Header.Get("X-Slack-Signature")
	if ts == "" || sig == "" {
		return false
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return false
	}
	if d := now.Unix() - sec; d > 300 || d < -300 {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "v0:%s:%s", ts, body)
	want := "v0=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(sig))
}

func (m *Mirror) handleInteractive(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "unreadable", http.StatusBadRequest)
		return
	}
	if !verifySlackSignature(m.signingSecret, r, body, m.now()) {
		// One answer for every verification failure: nothing about which
		// check failed leaks to whoever is probing.
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	// Slack sends application/x-www-form-urlencoded with a payload field.
	vals, err := url.ParseQuery(string(body))
	if err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	var payload struct {
		Type string `json:"type"`
		User struct {
			ID string `json:"id"`
		} `json:"user"`
		Actions []struct {
			ActionID string `json:"action_id"`
			Value    string `json:"value"`
		} `json:"actions"`
	}
	if err := json.Unmarshal([]byte(vals.Get("payload")), &payload); err != nil {
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	if payload.Type != "block_actions" || len(payload.Actions) == 0 {
		w.WriteHeader(http.StatusOK)
		return
	}

	for _, a := range payload.Actions {
		if a.ActionID != "stop_mirror" {
			continue
		}
		sessionID, groupID, ok := strings.Cut(a.Value, "|")
		if !ok {
			continue
		}
		// The presser must own the session. Their Slack id maps to an email
		// through the prefs cache; someone who never saved prefs has no
		// mapping and is refused — they also could not have started a mirror.
		var email string
		err := m.db.QueryRow(r.Context(), `
			SELECT s.email FROM sessions s
			  JOIN slack_prefs p ON p.email = s.email
			 WHERE s.session_id = $1 AND p.slack_user_id = $2`,
			sessionID, payload.User.ID).Scan(&email)
		if err != nil {
			m.log.Warn("slack interactive stop refused",
				"session_id", sessionID, "slack_user", payload.User.ID)
			continue
		}
		gid, err := strconv.ParseInt(groupID, 10, 64)
		if err != nil {
			continue
		}
		if err := detachSession(r.Context(), m.db, sessionID, gid, m.now()); err != nil {
			m.log.Error("slack interactive detach failed", "session_id", sessionID, "err", err)
			continue
		}
		m.log.Info("slack interactive detach", "session_id", sessionID, "group_id", gid, "by", email)
	}
	w.WriteHeader(http.StatusOK)
}
