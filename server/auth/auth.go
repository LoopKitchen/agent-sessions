// Package auth establishes who is calling the server and what they may read.
//
// Two kinds of caller arrive, and they cannot authenticate the same way. A
// person opening the dashboard is in front of a browser, so they sign in with
// the same company Google account and the same Firebase project the
// organisation's other services use, and carry a signed cookie for the rest of the visit. The
// service verifies the resulting Firebase ID token itself and mints its own
// cookie from it; it is not a Firebase client and holds no OAuth credential.
//
// One consequence is worth stating here rather than leaving to firebase.go. A
// Firebase ID token carries no `hd` claim, so there is no cryptographic proof
// of Workspace membership to lean on the way a Google OAuth token allowed.
// What stands in its place is the email domain check and the principals roster,
// which makes the roster the primary authorization control rather than a
// second opinion on one.
//
// The agent on their
// laptop uploads while nobody is watching, often days after the person last
// touched the machine, so it carries a long-lived device credential minted once
// during enrollment while that person was present. An interactive sign-in on
// every upload is not available to it, and a shared static key across the fleet
// would be one secret whose leak is indistinguishable from normal traffic.
//
// Everything here is written so that the identity a request carries is a value,
// not an ambient fact: verification returns an Identity, a DeviceIdentity or a
// Session, and the permission rule in authz.go is a pure function over those
// values. That makes the whole permission surface enumerable in a table test
// with no database and no network, which matters because the failure mode this
// layer must not have is the one nobody notices: quietly allowing a read of
// somebody else's transcript.
//
// Emails are the join key for every table in the schema, so they are normalised
// at every boundary by Normalize rather than compared raw.
package auth

import (
	"errors"
	"strings"
)

// Role is the whole authorization model: a handful of named administrators
// (bootstrapped from ADMIN_EMAILS, then managed on the admin page) who can see
// everything, and everyone else who sees their own work, which is what a small
// company actually needs; a manager hierarchy would encode an
// org chart that changes more often than this service is deployed.
type Role string

const (
	// RoleAdmin may read any session, and every such read is audited.
	RoleAdmin Role = "admin"
	// RoleMember may read their own sessions and anything explicitly shared
	// with them.
	RoleMember Role = "member"
)

// Valid reports whether r is one of the two roles the schema's CHECK constraint
// permits. An unrecognised role must never be treated as admin, so callers that
// read a role from anywhere outside the principals table should reject it here
// rather than defaulting it.
func (r Role) Valid() bool { return r == RoleAdmin || r == RoleMember }

// ParseRole converts stored text to a Role, rejecting anything unrecognised.
func ParseRole(s string) (Role, error) {
	r := Role(strings.ToLower(strings.TrimSpace(s)))
	if !r.Valid() {
		return "", errors.New("auth: unknown role")
	}
	return r, nil
}

// Normalize canonicalises an email for comparison and storage.
//
// Only case and surrounding whitespace are touched. RFC 5321 makes the local
// part case-sensitive in principle, but Google Workspace treats addresses
// case-insensitively and hands back whatever capitalisation the person typed,
// so a token that says Dev@Example.com has to match a principals row written as
// dev@example.com. What this deliberately does not do is strip dots or +tags:
// that is Gmail delivery behaviour, not identity, and folding them here would
// let two distinct Workspace accounts collapse into one principal.
func Normalize(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// SameEmail reports whether two addresses identify the same principal.
func SameEmail(a, b string) bool {
	na, nb := Normalize(a), Normalize(b)
	// An empty address is nobody. Without this an unauthenticated caller whose
	// email never got populated would match a session row whose owner column is
	// somehow empty, which is exactly the sort of accident that turns a bug in
	// one layer into a data leak in another.
	return na != "" && na == nb
}
