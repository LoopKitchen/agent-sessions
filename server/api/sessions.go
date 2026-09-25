package api

import (
	"net/http"
	"strings"
)

// sessionListResponse is the shape of GET /v1/sessions.
//
// Sessions is always an array, never null. A client that has to handle both
// eventually handles one of them wrong, and the one it gets wrong is the empty
// case, which is exactly what a new employee's first visit looks like.
type sessionListResponse struct {
	Sessions   []Session `json:"sessions"`
	NextCursor string    `json:"next_cursor,omitempty"`
}

// handleListSessions serves the list the dashboard opens on.
//
// The listing is scoped by the same permission rule as every other read, and it
// is the one read that is not audited: it carries rollup metadata rather than a
// transcript, and a row per listed session would make the access log unreadable
// at exactly the moment somebody is trying to read it carefully.
func (h *Handler) handleListSessions(w http.ResponseWriter, r *http.Request) {
	v, ok := h.viewer(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	f := SessionFilter{
		Query:  strings.TrimSpace(q.Get("q")),
		Email:  normalizeEmail(q.Get("email")),
		Source: strings.TrimSpace(q.Get("source")),
		Repo:   strings.TrimSpace(q.Get("repo")),
		Types:  parseTypes(q.Get("types")),
	}
	var err error
	if f.From, err = parseTime(q.Get("from")); err != nil {
		badRequest(w, "from", err)
		return
	}
	if f.To, err = parseTime(q.Get("to")); err != nil {
		badRequest(w, "to", err)
		return
	}
	if f.Limit, err = h.parseLimit(q.Get("limit")); err != nil {
		badRequest(w, "limit", err)
		return
	}

	// The viewer is part of the fingerprint, because a listing is scoped by who
	// is asking: the same position under two identities is two different pages,
	// and a cursor pasted from somebody else's URL should be refused rather
	// than quietly reinterpreted. The page size is deliberately not part of it.
	// A cursor names a row, not a window, so a reader who switches from twenty
	// rows to a hundred mid-scroll continues from where they were instead of
	// being sent back to the top.
	scope := scopeOf(string(cursorSessions), v.Email, f.Query, f.Email, f.Source, f.Repo,
		f.From.UTC().Format(cursorTimeLayout), f.To.UTC().Format(cursorTimeLayout),
		strings.Join(f.Types, ","))
	cur, err := decodeCursor(q.Get("cursor"), cursorSessions, scope)
	if err != nil {
		h.storeFailure(w, r, "list sessions", err)
		return
	}
	f.Cursor = cur.Store

	page, err := h.store.ListSessions(r.Context(), v, f)
	if err != nil {
		h.storeFailure(w, r, "list sessions", err)
		return
	}
	body := sessionListResponse{Sessions: page.Sessions}
	if body.Sessions == nil {
		body.Sessions = []Session{}
	}
	if page.NextCursor != "" {
		body.NextCursor = encodeCursor(cursor{Kind: cursorSessions, Scope: scope, Store: page.NextCursor})
	}
	writeJSON(w, http.StatusOK, body)
}

// parseTypes reads the comma-separated types parameter. Nothing is validated
// here: the store keeps the one list of values a type may hold and drops
// anything else before it reaches a WHERE clause, and a second copy of that
// list in this package would be a second list to keep in step. An absent
// parameter is an empty list, which the store reads as its default.
func parseTypes(raw string) []string {
	var out []string
	for _, t := range strings.Split(raw, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// sessionDetailResponse is the shape of GET /v1/sessions/{id}.
type sessionDetailResponse struct {
	Session Session `json:"session"`
	// Shares are the grants on this session. The store returns them only to the
	// owner and to administrators, so for a reader who arrived through a share
	// link this is empty: being given a session does not come with the right to
	// see who else was given it.
	Shares []Share `json:"shares"`
}

// handleGetSession serves one session's rollup, audited by the store when the
// session is not the viewer's own.
func (h *Handler) handleGetSession(w http.ResponseWriter, r *http.Request) {
	v, ok := h.viewer(w, r)
	if !ok {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeNotFound(w)
		return
	}

	sess, err := h.store.GetSession(r.Context(), v, id)
	if err != nil {
		h.storeFailure(w, r, "get session", err)
		return
	}
	shares, err := h.store.ListShares(r.Context(), v, id)
	if err != nil {
		// Failing the whole request rather than returning the session without
		// its shares: a share list that is silently empty when the query broke
		// is a page that tells its owner nobody can see this session.
		h.storeFailure(w, r, "list shares", err)
		return
	}
	if shares == nil {
		shares = []Share{}
	}
	writeJSON(w, http.StatusOK, sessionDetailResponse{Session: sess, Shares: shares})
}

// eventsResponse is the shape of GET /v1/sessions/{id}/events.
type eventsResponse struct {
	SessionID string        `json:"session_id"`
	Events    []StoredEvent `json:"events"`
	HasMore   bool          `json:"has_more"`
	// NextCursor is present only when there is another page. Emitting one for a
	// final page invites a round trip that is guaranteed to come back empty.
	NextCursor string `json:"next_cursor,omitempty"`
}

// handleGetEvents serves a window of a session's transcript in sequence order,
// audited by the store when the session is not the viewer's own.
//
// Ordering is by the harness's own sequence number rather than by arrival,
// because retries and backfill do not preserve arrival order and a transcript
// rendered in arrival order is not a transcript. That also makes the window
// stable while the session is still being written to: a sequence number is
// assigned once and never moves, so events arriving during a read are appended
// beyond the window rather than shifting it.
func (h *Handler) handleGetEvents(w http.ResponseWriter, r *http.Request) {
	v, ok := h.viewer(w, r)
	if !ok {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeNotFound(w)
		return
	}
	limit, err := h.parseLimit(r.URL.Query().Get("limit"))
	if err != nil {
		badRequest(w, "limit", err)
		return
	}
	// The session id is part of the fingerprint, so a cursor from one
	// transcript cannot be presented against another and land somewhere
	// arbitrary in it.
	scope := scopeOf(string(cursorEvents), v.Email, id)
	cur, err := decodeCursor(r.URL.Query().Get("cursor"), cursorEvents, scope)
	if err != nil {
		h.storeFailure(w, r, "get events", err)
		return
	}

	page, err := h.store.GetEvents(r.Context(), v, id, EventRange{AfterSeq: cur.AfterSeq, Limit: limit})
	if err != nil {
		h.storeFailure(w, r, "get events", err)
		return
	}
	body := eventsResponse{SessionID: id, Events: page.Events, HasMore: page.HasMore}
	if body.Events == nil {
		body.Events = []StoredEvent{}
	}
	if page.HasMore && page.NextAfter != nil {
		next := *page.NextAfter
		body.NextCursor = encodeCursor(cursor{Kind: cursorEvents, Scope: scope, AfterSeq: &next})
	}
	writeJSON(w, http.StatusOK, body)
}
