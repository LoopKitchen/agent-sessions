package auth

import "time"

// Via names the rule that allowed a read. It is written verbatim into the
// access_log.via column, so the values here are part of the storage contract
// rather than an internal enum.
type Via string

const (
	// ViaOwn is the viewer reading their own session. Not audited: logging
	// everybody reading their own work would bury the reads that matter under
	// noise, and there is no access to justify.
	ViaOwn Via = "own"
	// ViaAdmin is an administrator reading somebody else's session.
	ViaAdmin Via = "admin"
	// ViaShare is a read allowed by an explicit grant on that session.
	ViaShare Via = "share"
)

// Viewer is the authenticated caller. Role is carried alongside the email
// rather than looked up inside CanRead so that the decision stays a pure
// function of its inputs.
type Viewer struct {
	Email string
	Role  Role
}

// Grant is one row of the shares table as the decision needs to see it.
type Grant struct {
	// ID is carried through to Decision so the caller can say which share
	// allowed a read when it writes the audit row or answers a support question.
	ID string
	// Grantee is the person the share names. Empty means the share is open to
	// any authenticated employee, which is what the schema's nullable grantee
	// column expresses.
	Grantee string
	// ExpiresAt zero means the share does not expire.
	ExpiresAt time.Time
	// RevokedAt zero means the share is still live.
	RevokedAt time.Time
}

// Active reports whether a grant is usable at time now. Both timestamps are
// compared with "not before", so a share expiring at exactly now is already
// expired: at a boundary the safe answer is the one that denies.
func (g Grant) Active(now time.Time) bool {
	if !g.RevokedAt.IsZero() && !now.Before(g.RevokedAt) {
		return false
	}
	if !g.ExpiresAt.IsZero() && !now.Before(g.ExpiresAt) {
		return false
	}
	return true
}

// Decision is the outcome of the permission rule.
type Decision struct {
	// Allowed is the answer. A false Allowed must become a 404 at the HTTP
	// edge, never a 403: a 403 confirms the session exists, which tells the
	// caller that a named colleague ran something, and that is precisely the
	// fact they were not allowed to learn.
	Allowed bool
	// Via names the rule that allowed it, empty when denied.
	Via Via
	// Audit is true whenever somebody read work that is not their own. The
	// caller must write the access_log row in the same transaction as the read
	// and fail the read if that write fails, because unaudited access to a
	// colleague's transcript is worse than a failed page load.
	Audit bool
	// ShareID is set when Via is ViaShare.
	ShareID string
}

// CanRead applies section 2 of the server contract: own, then admin, then an
// active share, otherwise deny.
//
// The ordering is load-bearing in one case that looks like a tie. An
// administrator reading their own session is attributed to ViaOwn and not
// audited, because auditing an admin's own work would fill the access log with
// the four people most likely to be reading it and hide the reads an auditor is
// actually looking for.
//
// It is a pure function of its arguments, with the clock passed in, so every
// combination of role, ownership and share state can be enumerated in a table
// test. The alternative — resolving shares and roles inside a database query —
// makes the rule only observable through the storage layer, and a rule you can
// only test end to end is a rule that gets tested for the happy path.
func CanRead(v Viewer, owner string, grants []Grant, now time.Time) Decision {
	// Nobody is not somebody. An unauthenticated caller reaching this function
	// at all is a bug in the caller, but it must not be a permissive one.
	if Normalize(v.Email) == "" {
		return Decision{}
	}

	if SameEmail(v.Email, owner) {
		return Decision{Allowed: true, Via: ViaOwn}
	}

	if v.Role == RoleAdmin {
		return Decision{Allowed: true, Via: ViaAdmin, Audit: true}
	}

	for _, g := range grants {
		if !g.Active(now) {
			continue
		}
		// An empty grantee is the schema's NULL: shared with any authenticated
		// employee. It is reached only after the empty-viewer check above, so
		// "any employee" can never mean "any caller".
		if g.Grantee == "" || SameEmail(v.Email, g.Grantee) {
			return Decision{Allowed: true, Via: ViaShare, Audit: true, ShareID: g.ID}
		}
	}

	return Decision{}
}

// CanShare reports whether the viewer may create or revoke a share on a
// session. Only the owner or an administrator can, so a share received cannot
// be forwarded onward by its recipient: a chain of reshares would make the
// question "who can see this session" unanswerable without walking a graph, and
// the person who created the first share never agreed to that.
func CanShare(v Viewer, owner string) bool {
	if Normalize(v.Email) == "" {
		return false
	}
	return SameEmail(v.Email, owner) || v.Role == RoleAdmin
}
