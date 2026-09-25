package store

import (
	"context"
	"strings"
	"testing"
)

// TestClaudeTranscriptPathFollowsTheHarnessSlugRule pins the directory rule
// observed on this machine on 2026-09-11 (ls ~/.claude/projects against the
// cwd values of its own sessions): every character outside [A-Za-z0-9]
// becomes '-', so '/', '.', '_' and a space all do, and a segment that
// already starts with '-' yields '--'. The home directory comes off the cwd,
// and a cwd under neither /Users nor /home yields no path at all.
func TestClaudeTranscriptPathFollowsTheHarnessSlugRule(t *testing.T) {
	const id = "effb3df5-e7c3-47c4-a2b7-bbf050ea3e8c"
	cases := []struct{ cwd, want string }{
		// Three real cwd values of this machine's sessions, each matched
		// against the directory the harness made for it.
		{"/home/alex/work/api/apps/ai_ingestion",
			"/home/alex/.claude/projects/-home-alex-work-api-apps-ai-ingestion/" + id + ".jsonl"},
		{"/home/alex/.claude/projects/-home-alex-work-api/memory",
			"/home/alex/.claude/projects/-home-alex--claude-projects--home-alex-work-api-memory/" + id + ".jsonl"},
		{"/home/alex/work/api/.claude/worktrees/feature-a",
			"/home/alex/.claude/projects/-home-alex-work-api--claude-worktrees-feature-a/" + id + ".jsonl"},
		// The rule's remaining cases: a dot, an underscore and a space in one
		// path, a Linux home, and cwds the rule cannot place.
		{"/home/x/Downloads/Acme 2/my_repo.v2",
			"/home/x/.claude/projects/-home-x-Downloads-Acme-2-my-repo-v2/" + id + ".jsonl"},
		{"/home/dev/work", "/home/dev/.claude/projects/-home-dev-work/" + id + ".jsonl"},
		{"/private/tmp/claude-501/x/scratchpad", ""},
		{"/", ""},
		{"/home/", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := ClaudeTranscriptPath(c.cwd, id); got != c.want {
			t.Errorf("ClaudeTranscriptPath(%q) = %q, want %q", c.cwd, got, c.want)
		}
	}
	if got := ClaudeTranscriptPath("/home/x/work", ""); got != "" {
		t.Errorf("a path was built for an empty session id: %q", got)
	}
	if got := ClaudeProjectSlug("a b.c_d/e"); got != "a-b-c-d-e" {
		t.Errorf("slug = %q", got)
	}
}

// TestRepairListRefusesANonUUIDDevice: the device id reaches SQL as a uuid
// cast, and a bearer that resolved to nothing must not become a statement.
func TestRepairListRefusesANonUUIDDevice(t *testing.T) {
	db := &fakeDB{}
	if _, err := NewWithDB(db, nil).RepairList(context.Background(), "not-a-device", at, 200); err == nil {
		t.Fatal("RepairList accepted a device id that is not a uuid")
	}
	if len(db.calls) != 0 {
		t.Errorf("a refused device id reached the database: %d statements", len(db.calls))
	}
}

// TestSessionFacetsIsOneBatchedQuery: the list page's facets come from one
// statement over the page's ids, never a read per row, and an empty page
// asks nothing.
func TestSessionFacetsIsOneBatchedQuery(t *testing.T) {
	db := &fakeDB{}
	s := NewWithDB(db, nil)
	v := Viewer{Email: "me@example.com", Role: RoleMember}
	if got, err := s.SessionFacets(context.Background(), v, nil); err != nil || len(got) != 0 || len(db.calls) != 0 {
		t.Fatalf("an empty id list asked the database: %v %v %d", got, err, len(db.calls))
	}
	ids := []string{"s1", "s2", "s3"}
	if _, err := s.SessionFacets(context.Background(), v, ids); err != nil {
		t.Fatalf("SessionFacets: %v", err)
	}
	if len(db.calls) != 1 {
		t.Fatalf("issued %d statements for %d ids, want one", len(db.calls), len(ids))
	}
	c := db.calls[0]
	if !strings.Contains(c.sql, "session_id = ANY($3::text[])") {
		t.Errorf("facets are not read by id array:\n%s", c.sql)
	}
	if got, ok := c.args[2].([]string); !ok || len(got) != 3 {
		t.Errorf("id array = %v", c.args[2])
	}
	// Scoped to the viewer like every session read.
	if c.args[0] != false || c.args[1] != v.Email {
		t.Errorf("facets are not scoped to the viewer: %v", c.args[:2])
	}
}
