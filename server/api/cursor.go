package api

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// cursorTimeLayout renders a filter's timestamps for the query fingerprint. It
// keeps nanoseconds because two date filters a microsecond apart are two
// different queries, and folding them together would let a cursor from one be
// accepted by the other.
const cursorTimeLayout = time.RFC3339Nano

// Pagination here is a position, never a row count to skip.
//
// Sessions arrive continuously: an engineer's laptop uploads while somebody
// else is reading the list. Under an offset, a session ingested between two
// page requests shifts every later row down by one, so the reader sees one row
// twice and never sees the row it displaced. Neither is visible from the
// outside, which is what makes it worth avoiding rather than tolerating. A
// keyset position says "everything strictly older than this row", and rows
// arriving above it do not move it.
//
// The token is opaque and carries the position for whichever listing minted it,
// plus a fingerprint of the query it belongs to. The fingerprint is not
// security, it is honesty: a cursor is a position in one ordering produced by
// one filter, and replaying it against a different filter would produce a page
// that is neither the first nor the next one, silently. Rejecting it makes the
// caller's mistake visible at the moment they make it.
type cursorKind string

const (
	cursorSessions cursorKind = "sessions"
	cursorEvents   cursorKind = "events"
	cursorSearch   cursorKind = "search"
	cursorResume   cursorKind = "resume"
)

// cursor is the decoded token. Field names are short because the encoded form
// travels in a query string on every page request.
type cursor struct {
	Kind  cursorKind `json:"k"`
	Scope string     `json:"s"`

	// Store carries the storage layer's own keyset position for the session
	// list. This package does not interpret it: the ordering it encodes belongs
	// to whoever wrote the query, and re-deriving it here would be a second
	// definition of the same ordering.
	Store string `json:"c,omitempty"`

	// AfterSeq is the exclusive lower bound for a transcript window. Sequence
	// numbers within a session are assigned once and never move, so this is
	// stable even while the session is still being written to.
	AfterSeq *int64 `json:"a,omitempty"`

	// Offset is a rank position inside a search's candidate set. See
	// searchCursor for why this one is positional and the others are not.
	Offset int `json:"o,omitempty"`

	// Fork is the identifier minted for a resume bundle, carried across its
	// pages so that a transcript delivered in several requests assembles into
	// one file rather than into one file per page.
	Fork string `json:"f,omitempty"`
}

func encodeCursor(c cursor) string {
	b, err := json.Marshal(c)
	if err != nil {
		// cursor holds strings, an int and a pointer to an int; marshalling it
		// cannot fail, and returning an error here would put an unreachable
		// branch in every caller.
		panic("api: cursor is not serialisable: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeCursor parses a token and checks that it belongs to this listing. An
// empty token is the first page and not an error, which is what lets a caller
// pass the previous response's next_cursor back unconditionally.
func decodeCursor(raw string, kind cursorKind, scope string) (cursor, error) {
	if strings.TrimSpace(raw) == "" {
		return cursor{Kind: kind, Scope: scope}, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return cursor{}, ErrInvalidCursor
	}
	var c cursor
	if err := json.Unmarshal(b, &c); err != nil {
		return cursor{}, ErrInvalidCursor
	}
	if c.Kind != kind || c.Scope != scope {
		return cursor{}, ErrInvalidCursor
	}
	return c, nil
}

// scopeOf fingerprints the query a cursor belongs to. It is a hash rather than
// the fields themselves so that a token stays short and does not restate a
// colleague's email address in every page URL, which would put it in browser
// history and access logs for no benefit.
func scopeOf(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		// Length-prefixed so that adjacent fields cannot run together: an
		// email of "a" with a repo of "bc" must not fingerprint the same as
		// "ab" with "c".
		h.Write([]byte(strconv.Itoa(len(p))))
		h.Write([]byte{':'})
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}
