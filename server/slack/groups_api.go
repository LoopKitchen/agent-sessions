package slack

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
)

// The group routes. Two kinds of caller reach these: a person in a browser
// (the dashboard cookie, via Viewer) and a person's own machine (the device
// token the agent already holds for ingest, via DeviceEmail). Both resolve to
// an email and every authorization below is against that email — the CLI can
// do exactly what its owner can do in the browser, nothing more.

// RegisterGroups mounts the group and activation routes.
func (m *Mirror) RegisterGroups(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/slack/groups", m.handleListGroups)
	mux.HandleFunc("POST /v1/slack/groups", m.handleCreateGroup)
	mux.HandleFunc("PUT /v1/slack/groups/{id}", m.handleUpdateGroup)
	mux.HandleFunc("DELETE /v1/slack/groups/{id}", m.handleDeleteGroup)
	mux.HandleFunc("POST /v1/sessions/{id}/mirror/{group}", m.handleAttach)
	mux.HandleFunc("DELETE /v1/sessions/{id}/mirror/{group}", m.handleDetach)
}

// identity resolves the caller through either credential. The cookie is tried
// first because a browser can carry both; refusal names neither mechanism.
func (m *Mirror) identity(w http.ResponseWriter, r *http.Request) (string, bool) {
	if email, ok := m.viewer(r); ok && strings.TrimSpace(email) != "" {
		return strings.ToLower(strings.TrimSpace(email)), true
	}
	if m.deviceEmail != nil {
		if email, ok := m.deviceEmail(r); ok && strings.TrimSpace(email) != "" {
			return strings.ToLower(strings.TrimSpace(email)), true
		}
	}
	writeError(w, http.StatusUnauthorized, "unauthenticated", "sign in or present a device token")
	return "", false
}

// groupBody is what a caller may say about a group. No owner field: ownership
// is the caller's identity, always.
type groupBody struct {
	Name        string `json:"name"`
	Visibility  string `json:"visibility"`
	Destination string `json:"destination"`
	Disabled    *bool  `json:"disabled,omitempty"`
}

func (m *Mirror) decodeGroup(w http.ResponseWriter, r *http.Request) (groupBody, bool) {
	ct, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || ct != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "send application/json")
		return groupBody{}, false
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, maxPrefsBody))
	dec.DisallowUnknownFields()
	var b groupBody
	if err := dec.Decode(&b); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "the request body is not the expected JSON")
		return groupBody{}, false
	}
	return b, true
}

func validateGroup(b groupBody) (string, string, string, error) {
	name := strings.TrimSpace(b.Name)
	if name == "" || len(name) > 60 {
		return "", "", "", errors.New("name must be 1-60 characters")
	}
	vis := strings.TrimSpace(b.Visibility)
	if vis == "" {
		vis = "private"
	}
	if vis != "private" && vis != "org" {
		return "", "", "", errors.New(`visibility must be "private" or "org"`)
	}
	dest := strings.ToUpper(strings.TrimSpace(b.Destination))
	if strings.EqualFold(b.Destination, "dm") {
		dest = "dm"
	}
	if !ValidDestination(dest) {
		return "", "", "", errors.New(`destination must be "dm", a channel id (C… / G…) or a user id (U…)`)
	}
	return name, vis, dest, nil
}

func (m *Mirror) handleListGroups(w http.ResponseWriter, r *http.Request) {
	email, ok := m.identity(w, r)
	if !ok {
		return
	}
	gs, err := listGroups(r.Context(), m.db, email)
	if err != nil {
		m.failPrefs(w, r, "list groups", err)
		return
	}
	if gs == nil {
		gs = []Group{}
	}
	writeJSON(w, http.StatusOK, gs)
}

func (m *Mirror) handleCreateGroup(w http.ResponseWriter, r *http.Request) {
	email, ok := m.identity(w, r)
	if !ok {
		return
	}
	b, ok := m.decodeGroup(w, r)
	if !ok {
		return
	}
	name, vis, dest, err := validateGroup(b)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_group", err.Error())
		return
	}
	g, err := saveGroup(r.Context(), m.db, Group{
		Name: name, OwnerEmail: email, Visibility: vis, Destination: dest,
	}, m.now())
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			writeError(w, http.StatusConflict, "name_taken", "you already have a group with that name")
			return
		}
		m.failPrefs(w, r, "create the group", err)
		return
	}
	m.log.Info("slack group created", "email", email, "group", g.Name, "destination", g.Destination)
	writeJSON(w, http.StatusOK, g)
}

func (m *Mirror) handleUpdateGroup(w http.ResponseWriter, r *http.Request) {
	email, ok := m.identity(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_id", "the group id is not a number")
		return
	}
	b, ok := m.decodeGroup(w, r)
	if !ok {
		return
	}
	name, vis, dest, err := validateGroup(b)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_group", err.Error())
		return
	}
	g := Group{ID: id, Name: name, OwnerEmail: email, Visibility: vis, Destination: dest}
	if b.Disabled != nil {
		g.Disabled = *b.Disabled
	}
	g, err = saveGroup(r.Context(), m.db, g, m.now())
	if errors.Is(err, ErrGroupNotFound) {
		// Absent and not-yours are one answer, the same shape the session
		// reads use: a distinct "not yours" would confirm the group exists.
		writeError(w, http.StatusNotFound, "no_such_group", "no such group of yours")
		return
	}
	if err != nil {
		m.failPrefs(w, r, "update the group", err)
		return
	}
	writeJSON(w, http.StatusOK, g)
}

func (m *Mirror) handleDeleteGroup(w http.ResponseWriter, r *http.Request) {
	email, ok := m.identity(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_id", "the group id is not a number")
		return
	}
	if err := deleteGroup(r.Context(), m.db, email, id); errors.Is(err, ErrGroupNotFound) {
		writeError(w, http.StatusNotFound, "no_such_group", "no such group of yours")
		return
	} else if err != nil {
		m.failPrefs(w, r, "delete the group", err)
		return
	}
	m.log.Info("slack group deleted", "email", email, "group_id", id)
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "deleted": true})
}

// handleAttach binds the caller's own session to a group they can see.
func (m *Mirror) handleAttach(w http.ResponseWriter, r *http.Request) {
	email, ok := m.identity(w, r)
	if !ok {
		return
	}
	sessionID := r.PathValue("id")
	owned, err := m.ownsSession(r, sessionID, email)
	if err != nil {
		m.failPrefs(w, r, "check the session", err)
		return
	}
	if !owned {
		writeError(w, http.StatusNotFound, "no_such_session", "no such session of yours")
		return
	}
	g, err := resolveGroup(r.Context(), m.db, email, r.PathValue("group"))
	if errors.Is(err, ErrGroupNotFound) {
		writeError(w, http.StatusNotFound, "no_such_group", "no group by that name or id is visible to you")
		return
	}
	if err != nil {
		m.failPrefs(w, r, "resolve the group", err)
		return
	}
	if err := attachSession(r.Context(), m.db, sessionID, g.ID, email, m.now()); err != nil {
		m.failPrefs(w, r, "attach the session", err)
		return
	}
	m.log.Info("slack mirror attached", "email", email, "session_id", sessionID, "group", g.Name)
	writeJSON(w, http.StatusOK, map[string]any{"session_id": sessionID, "group": g.Name, "attached": true})
}

func (m *Mirror) handleDetach(w http.ResponseWriter, r *http.Request) {
	email, ok := m.identity(w, r)
	if !ok {
		return
	}
	sessionID := r.PathValue("id")
	owned, err := m.ownsSession(r, sessionID, email)
	if err != nil {
		m.failPrefs(w, r, "check the session", err)
		return
	}
	if !owned {
		writeError(w, http.StatusNotFound, "no_such_session", "no such session of yours")
		return
	}
	g, err := resolveGroup(r.Context(), m.db, email, r.PathValue("group"))
	if errors.Is(err, ErrGroupNotFound) {
		writeError(w, http.StatusNotFound, "no_such_group", "no group by that name or id is visible to you")
		return
	}
	if err != nil {
		m.failPrefs(w, r, "resolve the group", err)
		return
	}
	if err := detachSession(r.Context(), m.db, sessionID, g.ID, m.now()); err != nil {
		m.failPrefs(w, r, "detach the session", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session_id": sessionID, "group": g.Name, "attached": false})
}

// ownsSession reports whether a session belongs to email.
func (m *Mirror) ownsSession(r *http.Request, sessionID, email string) (bool, error) {
	var one int
	err := m.db.QueryRow(r.Context(),
		`SELECT 1 FROM sessions WHERE session_id = $1 AND email = $2`, sessionID, email).Scan(&one)
	if err == nil {
		return true, nil
	}
	if strings.Contains(err.Error(), "no rows") {
		return false, nil
	}
	return false, fmt.Errorf("slack: check session ownership: %w", err)
}
