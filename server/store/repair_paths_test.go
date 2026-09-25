package store

import (
	"context"
	"strings"
	"testing"
)

// TestTranscriptPathRefusesDotSegmentsAndNonUUIDIds is the adversarial
// pass's F4. The path is built from two strings a laptop sent; a dot
// segment in the cwd or a session id that is not the harness's uuid put a
// ".." into the path the client walks. A cwd that is not a clean absolute
// path under /Users or /home, or an id that is not a uuid, yields no path,
// and no emitted path ever carries a dot segment.
func TestTranscriptPathRefusesDotSegmentsAndNonUUIDIds(t *testing.T) {
	const id = "effb3df5-e7c3-47c4-a2b7-bbf050ea3e8c"
	for _, cwd := range []string{
		"/home/../etc", "/home/./x", "/home/x/../../etc", "/home/x/./y", "/home/x/..",
		"/home//x", "/home/x/", "Users/x", "/home/../root", "/home/./dev",
	} {
		if got := ClaudeTranscriptPath(cwd, id); got != "" {
			t.Errorf("ClaudeTranscriptPath(%q) = %q, want no path for an unclean cwd", cwd, got)
		}
	}
	for _, sid := range []string{
		"../../etc/passwd", "x", "effb3df5-e7c3-47c4-a2b7-bbf050ea3e8", id + "/../x", "rollout-2026-09-11T10-00-00-" + id,
	} {
		if got := ClaudeTranscriptPath("/home/x/work", sid); got != "" {
			t.Errorf("ClaudeTranscriptPath(session %q) = %q, want no path for an id that is not a uuid", sid, got)
		}
	}
	for _, cwd := range []string{"/home/x/work", "/home/x/a..b/c.d", "/home/dev/.claude/projects/-home-dev-x"} {
		got := ClaudeTranscriptPath(cwd, id)
		if got == "" {
			t.Errorf("ClaudeTranscriptPath(%q) built no path for a clean cwd", cwd)
		}
		for _, seg := range strings.Split(got, "/") {
			if seg == ".." || seg == "." {
				t.Errorf("ClaudeTranscriptPath(%q) = %q carries a dot segment", cwd, got)
			}
		}
	}
}

// TestProjectSlugCountsRunesLikeTheHarness is F5. Claude Code's rule runs
// over the JavaScript string, one dash per UTF-16 unit, so a two-byte rune
// is one dash and a rune above U+FFFF is two; a byte-wise rule made café
// into caf-- and would send the client to a directory that is not there.
func TestProjectSlugCountsRunesLikeTheHarness(t *testing.T) {
	cases := []struct{ cwd, want string }{
		{"/home/x/café", "-home-x-caf-"},
		{"/home/x/日本/repo", "-home-x----repo"},
		{"/home/x/a\U0001F600b", "-home-x-a--b"},
		{"/home/x/work/api", "-home-x-work-api"},
	}
	for _, c := range cases {
		if got := ClaudeProjectSlug(c.cwd); got != c.want {
			t.Errorf("ClaudeProjectSlug(%q) = %q, want %q", c.cwd, got, c.want)
		}
	}
}

// TestRepairListSettlesOnLastActivityAndCohortsDriveFromSessions pins F11
// and F10 at the statement. A session is settled when nothing has touched
// it for a day (updated_at, which every fold bumps), not when it started a
// day ago: a two-day session still running was listed and its transcript
// walked while being written. The missing-answer cohorts start from the
// sessions that began in the window (sessions_started_at_idx) and reach
// their turns through the turns primary key, never by scanning turns for a
// column it has no index on.
func TestRepairListSettlesOnLastActivityAndCohortsDriveFromSessions(t *testing.T) {
	db := &fakeDB{}
	s := NewWithDB(db, nil)
	if _, err := s.RepairList(context.Background(), "6f1c2a3b-4d5e-4f60-8a7b-9c0d1e2f3a4b", at, 200); err != nil {
		t.Fatalf("RepairList: %v", err)
	}
	if len(db.calls) != 1 {
		t.Fatalf("RepairList issued %d statements, want one", len(db.calls))
	}
	sql := db.calls[0].sql
	if !strings.Contains(sql, "s.updated_at < $3::timestamptz - interval '24 hours'") || strings.Contains(sql, "coalesce(s.ended_at, s.started_at)") {
		t.Errorf("the repair list does not settle on updated_at:\n%s", sql)
	}

	if _, err := s.FleetMissingAnswers(context.Background(), at); err != nil {
		t.Fatalf("FleetMissingAnswers: %v", err)
	}
	sql = db.calls[1].sql
	if !strings.Contains(sql, "FROM sessions s") || !strings.Contains(sql, "s.started_at >= $1") || strings.Contains(sql, "t.started_at >= $1") {
		t.Errorf("the cohort query does not drive from sessions.started_at:\n%s", sql)
	}
	if !strings.Contains(sql, "JOIN turns t ON t.session_id = s.session_id AND t.thread = ''") {
		t.Errorf("the cohort query does not reach turns through its primary key prefix:\n%s", sql)
	}
}
