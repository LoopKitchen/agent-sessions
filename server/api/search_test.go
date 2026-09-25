package api

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"
)

func searchStore(t *testing.T, n int) *fakeStore {
	t.Helper()
	f := seeded()
	for i := 1; i <= n; i++ {
		f.hits = append(f.hits, Hit{
			EventID:    fmt.Sprintf("e%d", i),
			SessionID:  "own-1",
			Email:      "member@example.com",
			Seq:        int64(i),
			Role:       "user",
			OccurredAt: testNow.Add(time.Duration(-i) * time.Minute),
			Snippet:    fmt.Sprintf("the migration failed on attempt %d", i),
			Rank:       float64(n-i) / 10,
		})
	}
	return f
}

func TestSearchNeedsSomethingToSearchFor(t *testing.T) {
	f := searchStore(t, 1)
	h := newHandler(t, f, &fakeAuth{email: "member@example.com"})

	for _, target := range []string{"/v1/search", "/v1/search?q=", "/v1/search?q=%20%20"} {
		w := do(t, h, http.MethodGet, target, "")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d, want 400", target, w.Code)
		}
	}
}

func TestSearchIsScopedToWhatTheViewerMayRead(t *testing.T) {
	f := searchStore(t, 2)
	// A hit in somebody else's session, which the viewer has no grant on.
	f.hits = append(f.hits, Hit{
		EventID: "x1", SessionID: "other-1", Email: "other@example.com",
		Seq: 1, Role: "assistant", OccurredAt: testNow, Snippet: "the migration failed on attempt 9",
	})
	h := newHandler(t, f, &fakeAuth{email: "member@example.com"})

	body := decodeBody[searchResponse](t, do(t, h, http.MethodGet, "/v1/search?q=migration", ""))
	if len(body.Hits) != 2 {
		t.Fatalf("hits = %d, want only the viewer's own two", len(body.Hits))
	}
	for _, hit := range body.Hits {
		if hit.SessionID == "other-1" {
			t.Fatal("a colleague's message reached a viewer with no right to it")
		}
	}
}

func TestSearchPassesItsFiltersThrough(t *testing.T) {
	f := searchStore(t, 1)
	h := newHandler(t, f, &fakeAuth{email: "admin@example.com"})

	q := url.Values{}
	q.Set("q", "migration")
	q.Set("email", "Member@Example.com")
	q.Set("source", "claude_code")
	q.Set("from", "2026-08-01T00:00:00Z")
	q.Set("limit", "7")
	if w := do(t, h, http.MethodGet, "/v1/search?"+q.Encode(), ""); w.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", w.Code, w.Body)
	}
	got := f.lastSearchFilter
	if got.Query != "migration" || got.Source != "claude_code" || got.Limit != 7 {
		t.Fatalf("filter = %+v", got)
	}
	if got.Email != "member@example.com" {
		t.Fatalf("email = %q, want normalised", got.Email)
	}
	if got.Offset != 0 {
		t.Fatalf("offset = %d, want the first page", got.Offset)
	}
}

func TestSearchPagesThroughTheRankedSet(t *testing.T) {
	f := searchStore(t, 5)
	h := newHandler(t, f, &fakeAuth{email: "member@example.com"})

	first := decodeBody[searchResponse](t, do(t, h, http.MethodGet, "/v1/search?q=migration&limit=2", ""))
	if len(first.Hits) != 2 || first.Candidates != 5 || first.Capped {
		t.Fatalf("first page = %+v", first)
	}
	if first.NextCursor == "" {
		t.Fatal("expected a next cursor while candidates remain")
	}

	second := decodeBody[searchResponse](t, do(t, h, http.MethodGet,
		"/v1/search?q=migration&limit=2&cursor="+url.QueryEscape(first.NextCursor), ""))
	if f.lastSearchFilter.Offset != 2 {
		t.Fatalf("offset = %d, want 2", f.lastSearchFilter.Offset)
	}
	if len(second.Hits) != 2 || second.Hits[0].EventID != "e3" {
		t.Fatalf("second page = %+v", second)
	}

	last := decodeBody[searchResponse](t, do(t, h, http.MethodGet,
		"/v1/search?q=migration&limit=2&cursor="+url.QueryEscape(second.NextCursor), ""))
	if len(last.Hits) != 1 || last.NextCursor != "" {
		t.Fatalf("last page = %+v, want one hit and no further cursor", last)
	}
}

// TestSearchStopsAtTheCandidateCap covers the honest end of a search. The store
// ranks a bounded slice of the match set, so paging has to stop where that slice
// does instead of walking off the end of it into empty pages.
func TestSearchStopsAtTheCandidateCap(t *testing.T) {
	f := searchStore(t, 5)
	f.candidateCap = 3
	h := newHandler(t, f, &fakeAuth{email: "member@example.com"})

	first := decodeBody[searchResponse](t, do(t, h, http.MethodGet, "/v1/search?q=migration&limit=2", ""))
	if first.Candidates != 3 || !first.Capped {
		t.Fatalf("first page = %+v, want a saturated candidate count", first)
	}
	second := decodeBody[searchResponse](t, do(t, h, http.MethodGet,
		"/v1/search?q=migration&limit=2&cursor="+url.QueryEscape(first.NextCursor), ""))
	if len(second.Hits) != 1 {
		t.Fatalf("second page = %+v", second)
	}
	if second.NextCursor != "" {
		t.Fatal("paging continued past the candidate cap")
	}
}

func TestSearchCursorBelongsToItsQuery(t *testing.T) {
	f := searchStore(t, 5)
	h := newHandler(t, f, &fakeAuth{email: "member@example.com"})

	first := decodeBody[searchResponse](t, do(t, h, http.MethodGet, "/v1/search?q=migration&limit=2", ""))
	w := do(t, h, http.MethodGet, "/v1/search?q=attempt&limit=2&cursor="+url.QueryEscape(first.NextCursor), "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestSearchHitsAreAlwaysAnArray(t *testing.T) {
	f := searchStore(t, 0)
	h := newHandler(t, f, &fakeAuth{email: "member@example.com"})

	body := decodeBody[searchResponse](t, do(t, h, http.MethodGet, "/v1/search?q=nothing-matches", ""))
	if body.Hits == nil {
		t.Fatal("hits decoded as null, want an empty array")
	}
}
