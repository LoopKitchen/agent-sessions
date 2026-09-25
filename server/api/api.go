// Package api serves the dashboard's read routes over the session store: the
// session list, one session's rollup, its transcript, full-text search, share
// links, and the bundle that lets somebody continue a colleague's session on
// their own machine.
//
// Three properties shape everything here, and each one defends against a
// specific way a system that collects colleagues' work goes wrong.
//
// Permission is decided per request, and a read of somebody else's session is
// recorded in the same database transaction as the read itself. Both live in
// the store: the rule is a predicate inside the SELECT and the audit row is
// written before the transaction commits, so a handler cannot forget to audit
// and an audit that fails takes its read down with it. This package therefore
// never re-derives the rule in Go. A second copy of a permission rule is a
// second thing to keep correct, and the copy that drifts is always the one that
// drifts more permissive. What the handlers own is the translation from a
// storage answer to a status code, which is the last place the property can be
// broken.
//
// A session the viewer may not see and a session that does not exist produce
// byte-identical responses. Answering 403 for the first would confirm that the
// session exists, and that a particular colleague ran something at a particular
// time is exactly the fact the viewer was not allowed to learn. The store
// reports both as ErrNotFound and nothing downstream is allowed to separate
// them again.
//
// Pagination is a keyset position rather than an offset. Sessions arrive
// continuously, including while somebody is scrolling the list they are in, and
// an offset silently repeats one row and drops another every time a session
// lands above the window.
package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Options configure a Handler.
type Options struct {
	// Store is the persistence the routes read. Required.
	Store Store
	// Auth verifies the caller's credential. Required, and supplied rather than
	// implemented here so that a change to how people sign in never touches a
	// route.
	Auth Authenticator

	// PageSize is the default listing window, and MaxPageSize the ceiling a
	// caller may ask for. An over-large request is clamped rather than
	// rejected: a caller being optimistic about a page size is not an error
	// worth failing their request over.
	PageSize    int
	MaxPageSize int

	// ResumeMaxEvents and ResumeMaxBytes bound one page of a resume bundle. A
	// transcript that exceeds either is delivered across several requests and
	// says so, rather than being silently shortened into a conversation that
	// never happened. The store applies its own ceiling as well, so a page can
	// be smaller than the ask; what the response reports is what it carries.
	ResumeMaxEvents int
	ResumeMaxBytes  int

	// DefaultShareTTL is how long a share lives when the caller names neither
	// an expiry nor a lifetime. Zero keeps the contract's behaviour, an
	// indefinite grant, and a deployment that wants links to lapse on their own
	// can impose it here without a code change.
	DefaultShareTTL time.Duration

	// Devices verifies device bearer tokens and Repair lists what a device
	// should re-walk; together they mount GET /v1/repair. Both optional: a
	// server built without them answers the route with the read API's 404,
	// which is what the client treats as "no list today".
	Devices DeviceAuthenticator
	Repair  RepairStore

	// SourceTokens mints the per-person laptop token a member's skill hook
	// posts under; with it POST /v1/source-tokens/laptop is mounted.
	// Optional for the reason Devices is: a server built without it
	// answers the route with the read API's 404.
	SourceTokens SourceTokenMinter

	// Sources verifies the publisher's token on PUT /v1/skill-catalog; with
	// it the route is mounted. When it is nil and Store also satisfies
	// SourceAuthenticator, the store is used, which is how the composition
	// root mounts the route without a second option.
	Sources SourceAuthenticator

	// Now is injectable so tests can place a request at a fixed moment instead
	// of racing the wall clock.
	Now func() time.Time

	// Logger receives the detail that 500 responses deliberately withhold. An
	// opaque error to the caller with nothing in the logs is an outage nobody
	// can diagnose.
	Logger *slog.Logger
}

const (
	defaultPageSize = 50
	// The store clamps its own limits as well; this ceiling exists so a caller
	// asking for a million rows gets a page rather than a timeout.
	defaultMaxPageSize = 500

	// Bodies on these routes are a handful of fields. The cap is here so a
	// mistargeted upload cannot be buffered into memory before being rejected.
	maxBodyBytes = 8 << 10

	// A resume page holds a few thousand transcript lines or a few megabytes,
	// whichever comes first. Both bounds are needed: sessions vary by three
	// orders of magnitude in bytes per event, so an event count alone permits a
	// response nobody can hold and a byte budget alone permits a response with
	// a hundred thousand tiny lines in it.
	defaultResumeMaxEvents = 2000
	defaultResumeMaxBytes  = 4 << 20
)

// Handler serves the read API.
type Handler struct {
	store       Store
	auth        Authenticator
	pageSize    int
	maxPageSize int
	resumeMaxN  int
	resumeMaxB  int
	shareTTL    time.Duration
	now         func() time.Time
	log         *slog.Logger
	mux         *http.ServeMux

	devices DeviceAuthenticator
	repair  RepairStore
	repairs repairLimiter
	tokens  SourceTokenMinter
	sources SourceAuthenticator
}

// New builds a Handler. It fails rather than defaulting when the store or the
// authenticator is missing, because a nil authenticator would make every
// transcript in the company public and that is not a condition to discover in
// production.
func New(o Options) (*Handler, error) {
	if o.Store == nil {
		return nil, errors.New("api: Store is required")
	}
	if o.Auth == nil {
		return nil, errors.New("api: Auth is required")
	}
	h := &Handler{
		store:       o.Store,
		auth:        o.Auth,
		pageSize:    o.PageSize,
		maxPageSize: o.MaxPageSize,
		resumeMaxN:  o.ResumeMaxEvents,
		resumeMaxB:  o.ResumeMaxBytes,
		shareTTL:    o.DefaultShareTTL,
		now:         o.Now,
		log:         o.Logger,
		devices:     o.Devices,
		repair:      o.Repair,
		tokens:      o.SourceTokens,
		sources:     o.Sources,
	}
	if h.sources == nil {
		if sa, ok := o.Store.(SourceAuthenticator); ok {
			h.sources = sa
		}
	}
	if h.maxPageSize <= 0 {
		h.maxPageSize = defaultMaxPageSize
	}
	if h.pageSize <= 0 {
		h.pageSize = defaultPageSize
	}
	if h.pageSize > h.maxPageSize {
		h.pageSize = h.maxPageSize
	}
	if h.resumeMaxN <= 0 {
		h.resumeMaxN = defaultResumeMaxEvents
	}
	if h.resumeMaxB <= 0 {
		h.resumeMaxB = defaultResumeMaxBytes
	}
	if h.now == nil {
		h.now = time.Now
	}
	if h.log == nil {
		h.log = slog.Default()
	}
	h.mux = http.NewServeMux()
	h.Register(h.mux)
	h.mux.HandleFunc("/", NotFound)
	return h, nil
}

// Register adds the read routes to a mux owned by the caller, for a server that
// mounts the admin API, the dashboard and this on one tree. The patterns are
// absolute, so this and ServeHTTP route identically and no route can be
// reachable through one and missing from the other. A caller that mounts these
// should install NotFound as the fallback on that mux for the reason given
// there.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/sessions", h.handleListSessions)
	mux.HandleFunc("GET /v1/sessions/{id}", h.handleGetSession)
	mux.HandleFunc("GET /v1/sessions/{id}/events", h.handleGetEvents)
	mux.HandleFunc("GET /v1/sessions/{id}/resume", h.handleResume)
	mux.HandleFunc("POST /v1/sessions/{id}/share", h.handleCreateShare)
	mux.HandleFunc("DELETE /v1/shares/{id}", h.handleRevokeShare)
	mux.HandleFunc("GET /v1/shared/{token}", h.handleResolveShare)
	mux.HandleFunc("GET /v1/search", h.handleSearch)
	mux.HandleFunc("GET /v1/skills/summary", h.handleSkillSummary)
	mux.HandleFunc("GET /v1/skills/invocations", h.handleSkillInvocations)
	mux.HandleFunc("GET /v1/skills/catalog/unused", h.handleSkillUnused)
	mux.HandleFunc("GET /v1/skills/unknown", h.handleSkillUnknown)
	mux.HandleFunc("GET /v1/skills/pruning", h.handleSkillPruning)
	mux.HandleFunc("GET /v1/skills/compliance", h.handleSkillCompliance)
	if h.devices != nil && h.repair != nil {
		mux.HandleFunc("GET /v1/repair", h.handleRepair)
	}
	if h.tokens != nil {
		mux.HandleFunc("POST /v1/source-tokens/laptop", h.handleMintLaptopToken)
	}
	if h.sources != nil {
		mux.HandleFunc("PUT /v1/skill-catalog/{source_repo}", h.handlePublishSkillCatalog)
	}
}

// ServeHTTP lets the Handler stand alone.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.mux.ServeHTTP(w, r) }

// NotFound writes this package's 404. It is exported so that a server mounting
// Register on its own mux can install the same fallback: an unknown path that
// answered with the standard library's plain-text 404 while a forbidden session
// answered with this JSON one would let a caller tell the two apart, which is
// the distinction the whole design is built to deny them.
//
// Installing it also absorbs method mismatches, so a verb a route does not
// serve reads as a route that does not exist. That is the intended shape here:
// the answer to "what is at this address" should not vary with how the question
// was asked.
func NotFound(w http.ResponseWriter, _ *http.Request) { writeNotFound(w) }

// ---------------------------------------------------------------------------
// Identity
// ---------------------------------------------------------------------------

// viewer resolves the caller, or writes the failure and reports that the
// request is over.
//
// The roster is re-read on every request. The credential proves who is calling
// and nothing else: a role baked into a cookie at sign-in outlives the
// revocation that was meant to end it, and on these routes that means somebody
// reading their colleagues' transcripts for the rest of the day after being
// demoted.
func (h *Handler) viewer(w http.ResponseWriter, r *http.Request) (Viewer, bool) {
	id, err := h.auth.Authenticate(r)
	if err != nil {
		if errors.Is(err, ErrNoIdentity) {
			// 401 rather than 404 here. Telling somebody they are not signed in
			// says nothing about anyone else, and answering 404 would send a
			// browser with an expired cookie into a dead end with no prompt to
			// sign in again.
			writeError(w, http.StatusUnauthorized, "unauthenticated", "sign in to continue")
			return Viewer{}, false
		}
		h.fail(r, "authenticate", err)
		writeInternal(w)
		return Viewer{}, false
	}
	email := normalizeEmail(id.Email)
	if email == "" {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "sign in to continue")
		return Viewer{}, false
	}

	p, found, err := h.store.Principal(r.Context(), email)
	if err != nil {
		h.fail(r, "read principal", err)
		writeInternal(w)
		return Viewer{}, false
	}
	// Both of these describe the caller to themselves, so they say plainly what
	// is wrong. Nothing here reveals anything about a colleague, which is why
	// this is the one authorization failure in the package that is not a 404.
	if !found {
		writeError(w, http.StatusForbidden, "not_enrolled",
			"this account is not enrolled in loop-sessions")
		return Viewer{}, false
	}
	if p.Disabled {
		writeError(w, http.StatusForbidden, "disabled", "this account has been disabled")
		return Viewer{}, false
	}
	if !p.Role.Valid() {
		// A roster row this package cannot interpret is a data problem, and
		// guessing at the role would either grant or withhold access on the
		// strength of a typo. Fail loudly instead, where an operator sees it.
		h.fail(r, "read principal", errors.New("roster row carries an unknown role"))
		writeInternal(w)
		return Viewer{}, false
	}
	return Viewer{Email: email, Role: p.Role}, true
}

// ---------------------------------------------------------------------------
// Responses
// ---------------------------------------------------------------------------

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeNotFound is the single 404. Every caller of it, whatever the reason,
// produces the same bytes: an unknown session id, a session belonging to
// somebody else, a share that was revoked an hour ago and a route that does not
// exist are indistinguishable from outside on purpose.
func writeNotFound(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, "not_found", "not found")
}

func writeInternal(w http.ResponseWriter) {
	writeError(w, http.StatusInternalServerError, "internal", "internal error")
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, errorBody{Error: errorDetail{Code: code, Message: msg}})
}

// writeJSON encodes into a buffer before touching the ResponseWriter. Encoding
// straight to the writer commits the status line first, so a marshal failure
// halfway through would append error text to a body already declared a success.
func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		setResponseHeaders(w)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"internal","message":"internal error"}}`))
		return
	}
	setResponseHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// setResponseHeaders applies what every response on these routes needs,
// whether or not it carries a body.
//
// Nothing here may be held by a shared cache: these bodies are colleagues'
// transcripts, and a proxy that kept one would serve it to whoever asked next,
// with no audit row and no permission check. The sniffing and referrer headers
// are the cheap half of the same argument. A JSON body full of transcript text
// that a browser decides to render as HTML is a stored cross-site scripting
// bug, and a session id in a Referer header sent to whatever a transcript links
// to is a session id in somebody else's logs.
func setResponseHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Cache-Control", "private, no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "same-origin")
}

// storeFailure turns an error from the store into a response.
//
// This is the whole of the leak defence at the HTTP edge, so it is one function
// rather than a switch repeated in every handler. Absence and denial arrive as
// the same sentinel and leave as the same 404; everything unrecognised is a 500
// whose detail goes to the log and not to the caller, because storage error
// text is written for operators and routinely names hosts, columns and
// constraints.
func (h *Handler) storeFailure(w http.ResponseWriter, r *http.Request, op string, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		writeNotFound(w)
	case errors.Is(err, ErrInvalidCursor):
		writeError(w, http.StatusBadRequest, "invalid_cursor", "cursor is not valid for this query")
	default:
		h.fail(r, op, err)
		writeInternal(w)
	}
}

// fail logs what the caller is not told. The method and path go with it,
// because a 500 with no route is an unanswerable page.
func (h *Handler) fail(r *http.Request, op string, err error) {
	h.log.Error("api request failed",
		slog.String("op", op),
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.String("error", err.Error()))
}

// ---------------------------------------------------------------------------
// Parameters
// ---------------------------------------------------------------------------

// normalizeEmail lower-cases and trims. Workspace addresses are case
// insensitive in practice, so without this a filter for Dev@Example.com would
// return nothing while the same person's sessions sit under dev@example.com.
func normalizeEmail(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// parseTime reads an RFC3339 timestamp. Only that format is accepted: a filter
// that silently interpreted "2026-08-04" as midnight in the server's zone would
// move a day boundary by hours for a fleet spread across time zones.
func parseTime(s string) (time.Time, error) {
	if strings.TrimSpace(s) == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, errors.New("must be an RFC3339 timestamp")
	}
	return t, nil
}

// parseLimit reads a page size, clamped to the configured ceiling.
func (h *Handler) parseLimit(raw string) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return h.pageSize, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, errors.New("must be a positive integer")
	}
	return min(n, h.maxPageSize), nil
}

// badRequest reports a caller error with the field that caused it, because
// "invalid request" on a route with six parameters costs somebody an afternoon.
func badRequest(w http.ResponseWriter, field string, err error) {
	writeError(w, http.StatusBadRequest, "invalid_request", field+": "+err.Error())
}
