package web

// Full-text search has no page of its own. It is a filter on the session list,
// because a second destination that lists sessions in a second layout is a
// second place to look for the same thing, and the one people reached for was
// not always the one that could answer their question.
//
// What the search page did well is the part that survives here: the store ranks
// matching messages and returns a snippet of each, so a reader sees the line
// that matched and lands on the transcript window containing it rather than at
// the top of a session with twelve thousand events in it.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// searchHit is one matching message, shaped for the list.
//
// It carries the session summary the store attached to the hit, which is
// thinner than a list row: enough to say which session matched, not enough to
// fill the row's columns. That is why a hit renders as a snippet on a row or
// beneath the table and never as a row of its own, where the counts and the
// cost would have to be invented.
type searchHit struct {
	Session Session
	URL     string
	Role    string
	// Kind is what matched, from messages.kind; Label is how the page says
	// it.
	Kind    string
	At      time.Time
	Seq     int64
	Snippet []Segment
}

// Label names the hit by what matched rather than by which side of the
// conversation it sat on: a person's words, a command, an answer, tool
// output, or one of the harness's own records by its kind.
func (h searchHit) Label() string {
	switch h.Kind {
	case "human", "":
		switch h.Role {
		case "assistant":
			return "assistant"
		case "tool":
			return "tool output"
		}
		return "prompt"
	case "slash_command":
		return "command"
	case "assistant_text", "assistant_usage_only":
		return "assistant"
	case "tool":
		return "tool output"
	}
	return strings.ReplaceAll(h.Kind, "_", " ")
}

// snippetWidth is how much text surrounds a hit. Wide enough to read the
// sentence the match sits in, narrow enough that fifty results still fit on a
// page someone can scan.
const snippetWidth = 320

// handleSearchRedirect answers the address the search page used to live at.
//
// A redirect rather than a 404 because the page existed and was used: links to
// it sit in browser histories and in pasted messages, and a bookmark that
// dead-ends teaches somebody the dashboard lost their query rather than that
// the query moved. Permanent, because the page is not coming back.
//
// It answers before resolving a viewer. Requiring one first would bounce an
// expired session to sign-in with /search as the return address, and the trip
// back would land here again; redirecting first means the sign-in they reach
// already carries the list URL they wanted. The redirect discloses nothing: it
// echoes the query the caller supplied.
func (s *Server) handleSearchRedirect(w http.ResponseWriter, r *http.Request) {
	target := "/sessions"
	if vals := parseFilters(r).values(); len(vals) > 0 {
		target += "?" + vals.Encode()
	}
	http.Redirect(w, r, target, http.StatusPermanentRedirect)
}

// messageMatches runs the message-granularity search behind the session list
// and keeps the strongest hit per session.
//
// One hit per session rather than all of them, because this feeds a list of
// sessions: a query matching forty times inside one transcript would otherwise
// push every other session out of view. The store returns hits in rank order,
// so the first one kept for a session is the best one it found.
//
// Matches are highlighted by splitting the text into marked and unmarked
// segments and letting the template emit the element, never by assembling HTML
// here. This is the one place on the dashboard where that shortcut is tempting,
// and it is also the text most likely to have come off the open internet.
func (s *Server) messageMatches(ctx context.Context, v Viewer, f filters) ([]searchHit, bool, error) {
	res, err := s.data.Search(ctx, v, SearchQuery{
		Q:      f.Q,
		Email:  f.Email,
		Source: f.Source,
		Types:  f.Types,
		// Repo is absent because messages carry no repository and the store
		// refuses to pretend otherwise. The caller does not reach this function
		// while a repo filter is set; see handleList.
		From:           parseDay(f.From, false),
		To:             parseDay(f.To, true),
		Limit:          s.pageN,
		IncludeHarness: f.Harness,
	})
	if err != nil {
		return nil, false, fmt.Errorf("web: search messages for %q: %w", f.Q, err)
	}

	terms := QueryTerms(f.Q)
	seen := make(map[string]bool, len(res.Hits))
	var out []searchHit
	for _, h := range res.Hits {
		if seen[h.Session.ID] {
			continue
		}
		seen[h.Session.ID] = true
		out = append(out, searchHit{
			Session: h.Session,
			URL:     hitURL(h, f.Q),
			Role:    h.Role,
			Kind:    h.Kind,
			At:      h.OccurredAt,
			Seq:     h.Seq,
			Snippet: Highlight(Snippet(h.Text, terms, snippetWidth), terms),
		})
	}
	return out, res.Capped, nil
}

// attachMatches folds hits into the rows they belong to and returns the rest.
//
// A hit whose session is already on the page belongs on that row: the row
// states what the session is and what it cost, and repeating it below the table
// would make one session read as two results. The rest cannot become rows,
// because a hit carries only the session's id and owner, so they are listed
// under the table with what is actually known about them. Dropping them instead
// would hide every transcript that matched on a word appearing nowhere in its
// session's own metadata, which is losing the search rather than folding it in.
func attachMatches(rows []sessionRow, hits []searchHit) ([]sessionRow, []searchHit) {
	byID := make(map[string]int, len(hits))
	for i, h := range hits {
		byID[h.Session.ID] = i
	}
	used := make([]bool, len(hits))
	for i := range rows {
		j, ok := byID[rows[i].S.ID]
		if !ok {
			continue
		}
		rows[i].Match = &hits[j]
		used[j] = true
	}
	var rest []searchHit
	for i, h := range hits {
		if !used[i] {
			rest = append(rest, h)
		}
	}
	return rows, rest
}

// hitURL lands the reader on the transcript page containing the match, with
// the query carried through so the match is still marked when they arrive.
// Linking to the top of the session instead would make search useless on
// exactly the long sessions it is for.
func hitURL(h SearchHit, q string) string {
	vals := url.Values{}
	if q != "" {
		vals.Set("hl", q)
	}
	// The scrollable reader with the event's own anchor: pages are turns
	// now rather than event windows, so the position is the event itself,
	// and the loader keeps fetching pages until the anchor arrives.
	u := "/sessions/" + url.PathEscape(h.Session.ID) + "/conversation"
	if len(vals) > 0 {
		u += "?" + vals.Encode()
	}
	if h.EventID != "" {
		u += "#ev-" + url.PathEscape(h.EventID)
	}
	return u
}
