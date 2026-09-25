package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// shareRequestBody is the body of POST /v1/sessions/{id}/share.
//
// Grantee and Anyone are separate fields rather than "an empty grantee means
// everybody", because the empty case is the weaker grant and a weaker grant
// must never be what a caller gets by leaving a field out of a request. A
// mistyped field name would otherwise turn "share with Alice" into "share with
// the company" and nothing about the response would look wrong.
type shareRequestBody struct {
	Grantee string `json:"grantee,omitempty"`
	Anyone  bool   `json:"anyone,omitempty"`

	// ExpiresAt and TTL are two ways to say the same thing, offered because a
	// dashboard has a date picker and a script has a duration. Sending both is
	// refused rather than resolved by precedence, since a caller that sent two
	// answers does not know what it is asking for.
	ExpiresAt string `json:"expires_at,omitempty"`
	TTL       string `json:"ttl,omitempty"`
}

type shareResponse struct {
	Share Share `json:"share"`
}

var (
	errShareAudience = errors.New(`set "grantee" to a colleague's address, or "anyone" to true to grant any signed-in employee`)
	errShareBothAud  = errors.New(`"grantee" and "anyone" cannot both be set`)
	errShareBothExp  = errors.New(`"expires_at" and "ttl" cannot both be set`)
	errSharePast     = errors.New("must be in the future")
	errShareTTL      = errors.New("must be a positive duration such as 72h")
	errShareEmail    = errors.New("must be an email address")
)

// handleCreateShare mints a share link for a session.
//
// Only the owner and administrators may share, which the store enforces; a
// caller who is neither is told the session does not exist rather than that
// they may not share it, for the same reason every other refusal here is a 404.
// That also means a share cannot be forwarded onward by the person who received
// it: a chain of reshares would make "who can see this session" a question
// nobody can answer without walking a graph, and the person who created the
// first share never agreed to that.
func (h *Handler) handleCreateShare(w http.ResponseWriter, r *http.Request) {
	v, ok := h.viewer(w, r)
	if !ok {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeNotFound(w)
		return
	}

	var body shareRequestBody
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	// An unknown field is refused rather than ignored, because the field a
	// caller misspells is usually the one that decides who can read the
	// session.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		badRequest(w, "body", errors.New("malformed: "+err.Error()))
		return
	}

	req, err := h.shareRequest(id, body)
	if err != nil {
		badRequest(w, "body", err)
		return
	}
	share, err := h.store.CreateShare(r.Context(), v, req)
	if err != nil {
		h.storeFailure(w, r, "create share", err)
		return
	}
	// 201 with the token in the body. The token is the entire credential for
	// whoever holds the link, so it is returned once to the person who asked
	// for it and never put in a URL this server logs.
	writeJSON(w, http.StatusCreated, shareResponse{Share: share})
}

// shareRequest validates the body and resolves the expiry. It is separate from
// the handler so the rules can be read, and tested, without a request.
func (h *Handler) shareRequest(sessionID string, body shareRequestBody) (ShareRequest, error) {
	grantee := normalizeEmail(body.Grantee)
	switch {
	case grantee != "" && body.Anyone:
		return ShareRequest{}, errShareBothAud
	case grantee == "" && !body.Anyone:
		return ShareRequest{}, errShareAudience
	}
	if grantee != "" && !plausibleEmail(grantee) {
		return ShareRequest{}, errShareEmail
	}

	req := ShareRequest{SessionID: sessionID, Grantee: grantee}
	now := h.now()
	switch {
	case body.ExpiresAt != "" && body.TTL != "":
		return ShareRequest{}, errShareBothExp
	case body.ExpiresAt != "":
		at, err := parseTime(body.ExpiresAt)
		if err != nil {
			return ShareRequest{}, err
		}
		if !at.After(now) {
			// A share that is already expired when it is created is a link that
			// will be handed to somebody and then not work, with no way to tell
			// from the outside whether it was revoked or never lived.
			return ShareRequest{}, errSharePast
		}
		req.ExpiresAt = at
	case body.TTL != "":
		d, err := time.ParseDuration(body.TTL)
		if err != nil || d <= 0 {
			return ShareRequest{}, errShareTTL
		}
		req.ExpiresAt = now.Add(d)
	case h.shareTTL > 0:
		req.ExpiresAt = now.Add(h.shareTTL)
	}
	return req, nil
}

// plausibleEmail rejects the obvious typo without pretending to validate an
// address. A grantee that is not an address at all mints a link nobody can ever
// use, and the person who created it finds out when a colleague tells them the
// page says the session does not exist.
func plausibleEmail(s string) bool {
	at := strings.IndexByte(s, '@')
	if at <= 0 || at == len(s)-1 {
		return false
	}
	if strings.ContainsAny(s, " \t\r\n,") {
		return false
	}
	return strings.Contains(s[at+1:], ".")
}

// handleRevokeShare withdraws a grant.
//
// Revocation is recorded rather than deleted by the store, so a link that was
// live during an incident can still be explained afterwards. A share that was
// already revoked, or was never the caller's to revoke, is reported as absent.
func (h *Handler) handleRevokeShare(w http.ResponseWriter, r *http.Request) {
	v, ok := h.viewer(w, r)
	if !ok {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeNotFound(w)
		return
	}
	if err := h.store.RevokeShare(r.Context(), v, id); err != nil {
		h.storeFailure(w, r, "revoke share", err)
		return
	}
	// No body: there is nothing left to describe, and a JSON envelope saying so
	// would only invite a client to parse it. The headers still go on, because
	// a revocation that a cache remembers as a success is a revocation the next
	// caller never performs.
	setResponseHeaders(w)
	w.WriteHeader(http.StatusNoContent)
}

type resolveShareResponse struct {
	Session Session `json:"session"`
	Share   Share   `json:"share"`
}

// handleResolveShare exchanges a share link for the session it grants, audited
// by the store.
//
// The holder of the link still has to be a signed-in employee. The token says
// which session the link is for; it does not say who is looking, and a read
// nobody can attribute is a read nobody can audit, which is the one thing this
// system promises not to have.
func (h *Handler) handleResolveShare(w http.ResponseWriter, r *http.Request) {
	v, ok := h.viewer(w, r)
	if !ok {
		return
	}
	token := strings.TrimSpace(r.PathValue("token"))
	if token == "" {
		writeNotFound(w)
		return
	}
	sess, share, err := h.store.ResolveShare(r.Context(), v, token)
	if err != nil {
		h.storeFailure(w, r, "resolve share", err)
		return
	}
	// The token is stripped from the echoed share. The caller already has it,
	// and a dashboard that renders this response would otherwise put the link
	// secret into a page that any screen share exposes.
	share.Token = ""
	writeJSON(w, http.StatusOK, resolveShareResponse{Session: sess, Share: share})
}
