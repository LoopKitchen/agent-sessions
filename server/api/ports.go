package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// Role is the whole authorization model: a handful of administrators who may
// read everything, and everybody else who reads their own work plus whatever
// has been shared with them.
type Role string

const (
	// RoleAdmin may read any session. Every such read is audited.
	RoleAdmin Role = "admin"
	// RoleMember may read their own sessions and anything explicitly granted.
	RoleMember Role = "member"
)

// Valid reports whether r is a role the roster is allowed to hold. An
// unrecognised value is never treated as a member either: a role this package
// cannot interpret means the roster says something we do not understand, and
// guessing at it is how a stale enum quietly grants or withholds access.
func (r Role) Valid() bool { return r == RoleAdmin || r == RoleMember }

// Identity is who the credential layer says is calling. It deliberately carries
// no role. A role that travelled with the cookie would be a role this package
// could not re-check, and the only moment that counts is the moment of the
// request: somebody demoted at lunchtime must not keep administrator reads
// until their cookie expires that evening.
type Identity struct {
	Email string
}

// Authenticator verifies the caller's credential. Implementations return
// ErrNoIdentity for a missing or unusable one and a real error for an
// infrastructure failure, because the first is the caller's problem and the
// second is ours.
type Authenticator interface {
	Authenticate(r *http.Request) (Identity, error)
}

// ErrNoIdentity means the request carried no usable credential.
var ErrNoIdentity = errors.New("api: no verified identity")

// Viewer is the identity a read is performed on behalf of, assembled per
// request from the credential plus the roster row it resolves to. It is a
// required argument on every store call that can expose somebody else's work,
// so "who is asking" cannot be omitted at a call site.
type Viewer struct {
	Email string
	Role  Role
}

// IsAdmin reports whether the viewer sees everything.
func (v Viewer) IsAdmin() bool { return v.Role == RoleAdmin }

// Principal is the roster row behind a caller. Only the parts this package acts
// on are here: what they may do, and whether they may do anything at all.
type Principal struct {
	Email       string `json:"email"`
	Role        Role   `json:"role"`
	DisplayName string `json:"display_name,omitempty"`
	// Disabled people keep their row so their sessions stay attributable and
	// the audit trail keeps its foreign keys. They simply cannot read anything.
	Disabled bool `json:"disabled,omitempty"`
}

// Store is everything this package needs from persistence.
//
// It is an interface rather than a concrete handle for a reason that outranks
// testability: authorization and audit are storage decisions here, not routing
// decisions. Every read takes the Viewer so the implementation can apply the
// permission rule inside the SELECT and write the access_log row in the same
// transaction as the read it records. A handler that fetched a session first
// and asked about permission afterwards would already have done the unaudited
// read by the time it found out, and a handler that re-derived the rule in Go
// would be a second copy of it, free to drift more permissive than the one the
// database actually applies.
//
// The corollary binds every implementation: no method may report "you may not
// see this" as an outcome distinguishable from "there is no such thing". Both
// are ErrNotFound. A distinct denial tells the caller that a particular session
// exists, which is itself a fact about a colleague's activity.
type Store interface {
	// Principal returns the roster entry for an email. The boolean separates
	// "nobody by that name" from a zero-valued row, which matters because a
	// zero row would read as an enabled member and answer authorization
	// questions on its own.
	Principal(ctx context.Context, email string) (Principal, bool, error)

	// ListSessions returns the sessions the viewer may see, newest first. It is
	// not audited: it carries rollup metadata rather than a transcript, and an
	// audit row per row of a paged list would bury the reads that matter under
	// the noise of somebody scrolling.
	ListSessions(ctx context.Context, v Viewer, f SessionFilter) (SessionPage, error)

	// GetSession returns one session's rollup. Audited.
	GetSession(ctx context.Context, v Viewer, sessionID string) (Session, error)

	// GetEvents returns a window of a session's events in sequence order.
	// Audited. The window exists because sessions reach hundreds of thousands
	// of events, and the ones people most want to read are the largest.
	GetEvents(ctx context.Context, v Viewer, sessionID string, r EventRange) (EventPage, error)

	// SearchMessages runs message-granularity full-text search scoped to what
	// the viewer may read. Audited once per distinct foreign session in the
	// page, because a hit renders a colleague's own words.
	SearchMessages(ctx context.Context, v Viewer, f SearchFilter) (SearchResult, error)

	// ListShares returns the grants on a session, for the owner or an admin.
	// Anyone else gets an empty list rather than an error, because the fact
	// that a session has been shared is part of what a share does not confer.
	ListShares(ctx context.Context, v Viewer, sessionID string) ([]Share, error)

	// CreateShare mints a grant. Only the owner or an admin may, and anybody
	// else must be told the session does not exist.
	CreateShare(ctx context.Context, v Viewer, req ShareRequest) (Share, error)

	// RevokeShare withdraws a grant, returning ErrNotFound when it is already
	// gone or was never the caller's to withdraw.
	RevokeShare(ctx context.Context, v Viewer, shareID string) error

	// ResolveShare exchanges a link secret for the session it grants. The
	// caller still has to be an authenticated employee: the token says which
	// session the link is for, it does not say who is looking, and a read
	// nobody can attribute is a read nobody can audit.
	ResolveShare(ctx context.Context, v Viewer, token string) (Session, Share, error)

	// The skill reads and the catalog publish (skills.go, skills_catalog.go)
	// are part of the same port: every read takes the Viewer so the store
	// scopes the person dimension inside the query, and a member's
	// compliance read comes back ErrNotFound like any other refusal.
	SkillStore
}

// SkillStore is the skill-usage half of Store (design 6.2, 4d): the six
// reads the /v1/skills routes serve, scoped by the store to the viewer, and
// the catalog publish the PUT route writes through.
type SkillStore interface {
	SkillSummary(ctx context.Context, v Viewer, f SkillFilter) (SkillSummary, error)
	SkillInvocations(ctx context.Context, v Viewer, f SkillFilter) (SkillInvocationPage, error)
	SkillUnused(ctx context.Context, v Viewer, f SkillFilter) ([]SkillUnusedRow, error)
	SkillUnknown(ctx context.Context, v Viewer, f SkillFilter) ([]SkillUnknownRow, error)
	SkillPruning(ctx context.Context, v Viewer, f SkillFilter) ([]SkillPruningRow, error)
	SkillCompliance(ctx context.Context, v Viewer, f SkillFilter) ([]SkillComplianceRow, error)
	// PublishSkillCatalog records one publish in one transaction. A
	// *CatalogFieldError, *LineageError, *AliasConflictError or
	// *CatalogShrinkError is the caller's; anything else is ours.
	PublishSkillCatalog(ctx context.Context, tokenID, sourceRepo string, body CatalogBody) (PublishResult, error)
}

// SourceAuthenticator verifies a source token (lss_) for the catalog route:
// the hash is resolved to its row and state, and the row is touched and
// counted when it is live for scope, in a transaction of its own that
// commits before the answer comes back. No row is a state, not an error.
type SourceAuthenticator interface {
	AuthenticateSource(ctx context.Context, tokenHash []byte, scope string) (SourceIdentity, error)
}

// SourceIdentity is a source token as the catalog route reads it: the row's
// id, platform, environment and scope, and the state the verify decided.
type SourceIdentity struct {
	ID, Platform, Environment, Scope string
	// State is live, or why the token is not: none, revoked, expired,
	// wrong_scope.
	State string
	// OverCap and SoftRevoked are the row's counter past its limit and the
	// limit-0 soft revoke.
	OverCap, SoftRevoked bool
}

// The token states the route acts on; the rest are all 401.
const (
	SourceStateNone = "none"
	SourceStateLive = "live"
)

// Errors the routes translate. Implementations of Store must use these rather
// than their own, because the mapping from an error to a status code is the
// last place the 404-not-403 rule can be broken.
var (
	// ErrNotFound means the row does not exist, or exists and the viewer has no
	// right to know that. It becomes a 404 in both cases.
	ErrNotFound = errors.New("api: not found")

	// ErrInvalidCursor means a pagination token was not one we issued for this
	// query. It is a 400 rather than a silent restart from the first page,
	// because a silently restarted page looks to the reader like duplicated
	// data and to a script like an endless list.
	ErrInvalidCursor = errors.New("api: invalid cursor")
)

// SessionFilter narrows a listing. The times are event times, so a date range
// means when the work happened rather than when it was uploaded, and a session
// backfilled from six months of history lands where it belongs.
type SessionFilter struct {
	Query  string
	Email  string
	Source string
	Repo   string
	// Types narrows to the named session types. Empty means the store's own
	// default, which leaves out the sessions in which nothing happened and the
	// harness's helper runs; a caller that wants those names them. The default
	// is the store's rather than this package's so the JSON API and the
	// dashboard cannot answer "which sessions" differently.
	Types []string
	From  time.Time
	To    time.Time
	Limit int
	// Cursor is the store's own keyset position, opaque to this package. It is
	// carried inside the cursor handed to the client rather than exposed
	// directly; see cursor.go.
	Cursor string
}

// SessionPage is one page of a listing plus the position the next page starts
// from.
type SessionPage struct {
	Sessions   []Session
	NextCursor string
}

// Session is the stored rollup as the dashboard sees it. The JSON tags are the
// wire contract: they are written out here rather than inherited from the
// storage struct so that renaming a column cannot silently rename a field the
// dashboard reads.
type Session struct {
	SessionID        string         `json:"session_id"`
	Email            string         `json:"email"`
	DeviceID         string         `json:"device_id,omitempty"`
	Source           string         `json:"source"`
	ParentSessionID  string         `json:"parent_session_id,omitempty"`
	Cwd              string         `json:"cwd,omitempty"`
	Repo             string         `json:"repo,omitempty"`
	GitBranch        string         `json:"git_branch,omitempty"`
	StartedAt        time.Time      `json:"started_at"`
	EndedAt          *time.Time     `json:"ended_at,omitempty"`
	Ended            bool           `json:"ended"`
	UserTurns        int            `json:"user_turns"`
	ToolCalls        int            `json:"tool_calls"`
	Subagents        int            `json:"subagents"`
	Errors           int            `json:"errors"`
	FirstPrompt      string         `json:"first_prompt,omitempty"`
	HarnessVersions  []string       `json:"harness_versions,omitempty"`
	TokensInput      int64          `json:"tokens_input"`
	TokensOutput     int64          `json:"tokens_output"`
	TokensCacheRead  int64          `json:"tokens_cache_read"`
	TokensCacheWrite int64          `json:"tokens_cache_write"`
	CostUSD          float64        `json:"cost_usd"`
	Redactions       map[string]int `json:"redactions,omitempty"`
	// IngestedAt is when the record reached the server, kept apart from
	// StartedAt so a backfilled session reads as historical rather than as
	// something that happened this morning.
	IngestedAt time.Time `json:"ingested_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// EventRange selects a slice of a session's events.
type EventRange struct {
	// AfterSeq is an exclusive lower bound, and nil means start at the
	// beginning. The distinction is load-bearing rather than stylistic: the
	// backfill walker numbers a session's events from zero, so treating a zero
	// bound as "from the start" would hide the first event of every imported
	// session and nothing about the result would look wrong.
	AfterSeq *int64
	Limit    int
	// IncludeSuperseded returns the hook-origin copies the derive runner
	// elected out in favour of their transcript twins. Off, a page reads as
	// the timeline does; on, it is every stored row, which is what the
	// resume bundle reads so its manifest counts the same events before and
	// after a fold.
	IncludeSuperseded bool
}

// EventPage is one page of a session's events in sequence order.
type EventPage struct {
	Events  []StoredEvent
	HasMore bool
	// NextAfter feeds straight back into EventRange.AfterSeq.
	NextAfter *int64
}

// StoredEvent is one row of the durable record. Body is the scrubbed event
// exactly as it was delivered, passed through rather than re-marshalled, so a
// reader sees what actually arrived instead of this package's idea of it.
type StoredEvent struct {
	ID         string          `json:"id"`
	SessionID  string          `json:"session_id"`
	Email      string          `json:"email"`
	Seq        int64           `json:"seq"`
	Type       string          `json:"type"`
	Origin     string          `json:"origin"`
	OccurredAt time.Time       `json:"occurred_at"`
	IngestedAt time.Time       `json:"ingested_at"`
	AgentID    string          `json:"agent_id,omitempty"`
	WorkflowID string          `json:"workflow_id,omitempty"`
	Model      string          `json:"model,omitempty"`
	ToolName   string          `json:"tool_name,omitempty"`
	Body       json.RawMessage `json:"body"`
}

// SearchFilter narrows a full-text search. Every field except Query is a cheap
// predicate applied before ranking, which is what keeps the query inside its
// latency budget.
type SearchFilter struct {
	Query  string
	Email  string
	Source string
	// Types mirrors SessionFilter.Types: empty means the store default.
	Types []string
	From  time.Time
	To    time.Time
	Limit int
	// Offset is a position inside the ranked candidate set rather than into the
	// table. It is not exposed on the wire; see cursor.go for why search paging
	// is positional and the other listings are not.
	Offset int
}

// Hit is one matching message.
type Hit struct {
	EventID    string    `json:"event_id"`
	SessionID  string    `json:"session_id"`
	Email      string    `json:"email"`
	Seq        int64     `json:"seq"`
	Role       string    `json:"role"`
	OccurredAt time.Time `json:"occurred_at"`
	Snippet    string    `json:"snippet"`
	Rank       float64   `json:"rank"`
}

// SearchResult is a page of hits.
type SearchResult struct {
	Hits []Hit
	// Candidates is how many matches were considered, saturating at the store's
	// candidate cap.
	Candidates int
	// Capped reports that the cap was reached, so the caller can say "500+"
	// honestly instead of presenting a truncated count as a total.
	Capped bool
}

// Share is an explicit grant on one session, on top of the role model.
type Share struct {
	ID        string     `json:"id"`
	SessionID string     `json:"session_id"`
	CreatedBy string     `json:"created_by"`
	Grantee   string     `json:"grantee,omitempty"`
	Token     string     `json:"token,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

// ShareRequest asks for a grant. An empty Grantee means any authenticated
// employee holding the link, which is the weaker of the two grants, so the
// route requires a caller to ask for it in so many words rather than arriving
// at it by leaving a field out.
type ShareRequest struct {
	SessionID string
	Grantee   string
	ExpiresAt time.Time
}
