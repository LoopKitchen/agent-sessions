package store

import (
	"context"
	"strings"
	"testing"
)

// TestSearchExcludesHarnessKindsAndWeightsByKindInOneStatement pins the two
// halves of the kind-aware search to a single query: the harness's wrappers
// leave the candidate set unless asked for, and the rank is weighted by what
// matched inside the same statement, never by a second round trip per hit.
func TestSearchExcludesHarnessKindsAndWeightsByKindInOneStatement(t *testing.T) {
	for _, include := range []bool{false, true} {
		db := &fakeDB{}
		s := NewWithDB(db, nil)
		if _, err := s.SearchMessages(context.Background(),
			Viewer{Email: "me@example.com", Role: RoleAdmin},
			SearchFilter{Query: "deploy", IncludeHarness: include}); err != nil {
			t.Fatalf("SearchMessages: %v", err)
		}
		var searches []call
		for _, c := range db.calls {
			if c.kind == "query" && strings.Contains(c.sql, "FROM messages m") {
				searches = append(searches, c)
			}
		}
		if len(searches) != 1 {
			t.Fatalf("issued %d message queries, want exactly one (no per-hit round trip)", len(searches))
		}
		c := searches[0]
		for _, want := range []string{
			"m.kind IN ('', 'human', 'slash_command')",
			"$12::bool OR m.role <> 'user'",
			// An internal session's harness prompt is its only content, so a
			// caller who asked for internal sessions is not refused it.
			"OR s.session_type = 'internal'",
			"WHEN c.kind IN ('human', 'slash_command') THEN 2.0",
			"t.final_event_id = c.event_id) THEN 1.8",
			"WHEN c.role = 'assistant' THEN 1.3",
			"ELSE 1.0",
		} {
			if !strings.Contains(c.sql, want) {
				t.Errorf("search lacks %q:\n%s", want, c.sql)
			}
		}
		if got := c.args[11]; got != include {
			t.Errorf("include-harness parameter = %v, want %v", got, include)
		}
		// The boost is inside the ranked CTE, over the capped candidates, so
		// the weighting costs a CASE per candidate and not a scan per row.
		if strings.Index(c.sql, "ranked AS") > strings.Index(c.sql, "THEN 2.0") {
			t.Errorf("the kind boost is applied before the candidate cap:\n%s", c.sql)
		}
	}
}

// TestSearchHitsCarryTheirKind: the label a result renders comes from the
// row, so the page never re-classifies text at render time.
func TestSearchHitsCarryTheirKind(t *testing.T) {
	rows := [][]any{
		{"ev1", "sess", "me@example.com", int64(1), "user", "slash_command", at, "snip", 0.9, int64(1)},
	}
	db := &fakeDB{stubs: []*stub{{match: "ts_rank", rows: rows}}}
	res, err := NewWithDB(db, nil).SearchMessages(context.Background(), Viewer{Email: "me@example.com"},
		SearchFilter{Query: "deploy"})
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if len(res.Hits) != 1 || res.Hits[0].Kind != "slash_command" {
		t.Fatalf("hits = %+v, want the kind carried through", res.Hits)
	}
}
