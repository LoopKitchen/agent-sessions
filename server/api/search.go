package api

import (
	"errors"
	"net/http"
	"strings"
)

// errNoQuery is the refusal to run a search with nothing to search for.
var errNoQuery = errors.New("a search needs something to search for")

// searchResponse is the shape of GET /v1/search.
type searchResponse struct {
	Query string `json:"query"`
	Hits  []Hit  `json:"hits"`
	// Candidates is how many matching messages were considered. It saturates at
	// the store's candidate cap, which is what Capped reports, so a client can
	// render "500+" rather than presenting a bounded number as a total.
	Candidates int    `json:"candidates"`
	Capped     bool   `json:"capped"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// handleSearch serves message-granularity full-text search.
//
// The shape of this route is fixed by a property of Postgres rather than by
// preference. Ranking has to read the stored tsvector of every row it scores,
// so it is the part that scales with the size of the match set, and a common
// word across a whole fleet's corpus matches far more rows than any page will
// show. The permission scope and the cheap predicates therefore run first, the
// survivors are capped, only the capped set is ranked, and snippet generation,
// which re-parses the document text and is dearer still, touches only the rows
// actually being returned. Ranking before filtering would spend the entire
// latency budget scoring rows the viewer is not allowed to see.
//
// Results are audited, one row per distinct foreign session in the page, in the
// same transaction as the read. A hit renders a colleague's own words through a
// snippet, so it exposes content and the same rule applies to it as to opening
// their transcript.
func (h *Handler) handleSearch(w http.ResponseWriter, r *http.Request) {
	v, ok := h.viewer(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	f := SearchFilter{
		Query:  strings.TrimSpace(q.Get("q")),
		Email:  normalizeEmail(q.Get("email")),
		Source: strings.TrimSpace(q.Get("source")),
		Types:  parseTypes(q.Get("types")),
	}
	if f.Query == "" {
		// An empty query would match everything and rank none of it. Answering
		// with the whole corpus would also be the one read that hands over
		// colleagues' message text without anybody having asked for anything.
		badRequest(w, "q", errNoQuery)
		return
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

	scope := scopeOf(string(cursorSearch), v.Email, f.Query, f.Email, f.Source,
		f.From.UTC().Format(cursorTimeLayout), f.To.UTC().Format(cursorTimeLayout),
		strings.Join(f.Types, ","))
	cur, err := decodeCursor(q.Get("cursor"), cursorSearch, scope)
	if err != nil {
		h.storeFailure(w, r, "search", err)
		return
	}
	f.Offset = cur.Offset

	res, err := h.store.SearchMessages(r.Context(), v, f)
	if err != nil {
		h.storeFailure(w, r, "search", err)
		return
	}
	body := searchResponse{
		Query:      f.Query,
		Hits:       res.Hits,
		Candidates: res.Candidates,
		Capped:     res.Capped,
	}
	if body.Hits == nil {
		body.Hits = []Hit{}
	}
	// Paging inside a search is a rank position, not a keyset position, and it
	// is the one listing here that cannot be made stable. Rank is a total order
	// over a candidate set that is recomputed per request, so a message
	// ingested between two pages can displace a boundary row. The alternative
	// would be to freeze the candidate set server-side per query, which means
	// holding a colleague's search results in memory between requests, and the
	// trade is not worth it for a page of twenty. The cursor is opaque so the
	// property can change later without a client change.
	//
	// The next position is offered only while the candidate set still has rows
	// beyond this page. Because Candidates saturates at the store's cap, this
	// also stops paging at the cap rather than walking off the end of it.
	if next := f.Offset + len(res.Hits); len(res.Hits) > 0 && next < res.Candidates {
		body.NextCursor = encodeCursor(cursor{Kind: cursorSearch, Scope: scope, Offset: next})
	}
	writeJSON(w, http.StatusOK, body)
}
